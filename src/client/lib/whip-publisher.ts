// WHIP screen publisher: shares the screen to the server-side stream bridge
// so an external HLS player (VRChat's AVPro) can watch it. The server proxies
// the signaling to MediaMTX and injects the publish credentials, so the
// browser only ever talks to its own origin; ICE media then flows directly to
// the ingest endpoint's public UDP port.
//
// VP8 is preferred over H264 on purpose: some GPU/driver combinations make
// Chrome's hardware H264 encoder emit black frames for screen capture (the
// local preview stays fine because it uses the render path), while the server
// transcodes either codec to H264+AAC for the HLS output.

const WHIP_ENDPOINT = "/api/whip";
const ICE_GATHER_TIMEOUT_MS = 2_000;
const FALLBACK_VIDEO_BITRATE_BPS = 5_000_000;

// The bridge re-encodes to H264 on a 4 Mbps uplink, so asking the browser for
// far more than that only spends the host's upload on bits the server drops.
const MAX_USEFUL_VIDEO_BITRATE_BPS = 6_000_000;

/**
 * Send-side limits for the WHIP leg. They mirror the quality settings the host
 * picked for Piik's own share: resolution and frame rate already come along
 * because the publisher reuses the shared track, but the bitrate ceilings live
 * on the RTP sender and have to be applied to this peer connection too.
 */
export interface PublishLimits {
  videoMaxBitrateBps?: number;
  audioMaxBitrateBps?: number;
}

function clampBitrate(
  requested: number | undefined,
  fallback: number,
  ceiling: number,
): number {
  if (!Number.isFinite(requested) || (requested ?? 0) <= 0) {
    return fallback;
  }
  return Math.min(requested as number, ceiling);
}

function preferVP8(peerConnection: RTCPeerConnection): void {
  const getCapabilities = RTCRtpSender.getCapabilities?.bind(RTCRtpSender);
  const capabilities = getCapabilities?.("video");
  if (!capabilities) {
    return;
  }
  const rank = (codec: RTCRtpCodecCapability): number => {
    const mime = codec.mimeType.toLowerCase();
    if (mime === "video/vp8") return 2;
    if (mime === "video/h264") return 0;
    return 1;
  };
  const ordered = [...capabilities.codecs].sort((a, b) => rank(b) - rank(a));
  for (const transceiver of peerConnection.getTransceivers()) {
    if (transceiver.sender.track?.kind !== "video") {
      continue;
    }
    try {
      transceiver.setCodecPreferences(ordered);
    } catch {
      // Negotiation falls back to the browser's default codec order.
    }
  }
}

// MediaMTX's WHIP endpoint does not do trickle ICE: the POSTed offer must
// already carry the candidates. Wait for the gather to finish, with a safety
// timeout so a host without candidates still sends the offer.
function gathered(peerConnection: RTCPeerConnection, timeoutMs: number): Promise<void> {
  if (peerConnection.iceGatheringState === "complete") {
    return Promise.resolve();
  }
  return new Promise((resolve) => {
    const done = (): void => {
      peerConnection.removeEventListener("icegatheringstatechange", onChange);
      window.clearTimeout(timer);
      resolve();
    };
    const onChange = (): void => {
      if (peerConnection.iceGatheringState === "complete") {
        done();
      }
    };
    const timer = window.setTimeout(done, timeoutMs);
    peerConnection.addEventListener("icegatheringstatechange", onChange);
  });
}

export type ScreenPublishFailure = "capture" | "network" | "rejected";

export class ScreenPublishError extends Error {
  readonly reason: ScreenPublishFailure;

  constructor(reason: ScreenPublishFailure) {
    super(reason);
    this.reason = reason;
  }
}

export class ScreenPublisher {
  private peerConnection: RTCPeerConnection | null = null;
  private stream: MediaStream | null = null;
  private sessionUrl: string | null = null;
  private stopping = false;
  private ownedStream = false;

  get active(): boolean {
    return this.stream !== null;
  }

  /**
   * Publishes `source` — normally the stream Piik is already sharing, so the
   * host is never asked to pick the screen twice. When `source` is null the
   * publisher captures one itself and owns it (stopping then ends the
   * capture as well). `onEnded` fires when the capture ends by itself, either
   * from the browser's "stop sharing" bar or because the host stopped sharing
   * from Piik's own controls — the caller resets its state; no further stop()
   * is needed.
   */
  async start(
    source: MediaStream | null,
    onPreview: (stream: MediaStream) => void,
    onEnded: () => void,
    limits: PublishLimits = {},
  ): Promise<void> {
    let stream = source;
    this.ownedStream = false;
    if (!stream) {
      try {
        stream = await navigator.mediaDevices.getDisplayMedia({
          video: { frameRate: 30 },
          audio: true,
        });
        this.ownedStream = true;
      } catch {
        throw new ScreenPublishError("capture");
      }
    }
    this.stopping = false;
    this.stream = stream;

    const peerConnection = new RTCPeerConnection();
    this.peerConnection = peerConnection;
    stream.getTracks().forEach((track) => peerConnection.addTrack(track, stream));
    preferVP8(peerConnection);
    stream.getVideoTracks()[0]?.addEventListener("ended", () => {
      // The user stopped sharing from the browser's own UI. Release the
      // session but swallow errors — the caller only resets its state.
      void this.stop().catch(() => undefined).finally(() => {
        if (!this.stopping) onEnded();
      });
    });

    try {
      const offer = await peerConnection.createOffer();
      await peerConnection.setLocalDescription(offer);
      await gathered(peerConnection, ICE_GATHER_TIMEOUT_MS);

      let response: Response;
      try {
        response = await fetch(WHIP_ENDPOINT, {
          method: "POST",
          headers: { "Content-Type": "application/sdp" },
          body: peerConnection.localDescription?.sdp ?? "",
        });
      } catch {
        throw new ScreenPublishError("network");
      }
      if (!response.ok) {
        throw new ScreenPublishError("rejected");
      }
      const answer = await response.text();
      await peerConnection.setRemoteDescription({ type: "answer", sdp: answer });

      const location = response.headers.get("Location");
      this.sessionUrl = location
        ? new URL(location, window.location.origin).toString()
        : null;

      // Apply the host's ceilings to this leg too, so the quality settings the
      // host sees in the UI govern the m3u8 output and not only the peer share.
      for (const sender of peerConnection.getSenders()) {
        const kind = sender.track?.kind;
        if (kind !== "video" && kind !== "audio") {
          continue;
        }
        const ceiling = kind === "video"
          ? clampBitrate(limits.videoMaxBitrateBps, FALLBACK_VIDEO_BITRATE_BPS,
            MAX_USEFUL_VIDEO_BITRATE_BPS)
          : limits.audioMaxBitrateBps;
        if (ceiling === undefined) {
          continue;
        }
        try {
          const parameters = sender.getParameters();
          if (parameters.encodings.length === 0) {
            parameters.encodings = [{}];
          }
          parameters.encodings[0].maxBitrate = ceiling;
          await sender.setParameters(parameters);
        } catch {
          // The negotiation works without the cap.
        }
      }
    } catch (error) {
      await this.stop().catch(() => undefined);
      throw error;
    }

    onPreview(stream);
  }

  /**
   * Re-applies send-side ceilings to a live session, so raising the bitrate
   * mid-share takes effect without restarting the publish. Resolution and
   * frame rate follow the shared track on their own.
   */
  async applyLimits(limits: PublishLimits): Promise<void> {
    const peerConnection = this.peerConnection;
    if (!peerConnection) {
      return;
    }
    for (const sender of peerConnection.getSenders()) {
      const kind = sender.track?.kind;
      if (kind !== "video" && kind !== "audio") {
        continue;
      }
      const ceiling = kind === "video"
        ? clampBitrate(limits.videoMaxBitrateBps, FALLBACK_VIDEO_BITRATE_BPS,
          MAX_USEFUL_VIDEO_BITRATE_BPS)
        : limits.audioMaxBitrateBps;
      if (ceiling === undefined) {
        continue;
      }
      try {
        const parameters = sender.getParameters();
        if (parameters.encodings.length === 0) {
          parameters.encodings = [{}];
        }
        parameters.encodings[0].maxBitrate = ceiling;
        await sender.setParameters(parameters);
      } catch {
        // The session keeps running with its previous ceiling.
      }
    }
  }

  async stop(): Promise<void> {
    this.stopping = true;
    const sessionUrl = this.sessionUrl;
    this.sessionUrl = null;
    if (sessionUrl) {
      // Best effort: MediaMTX reaps the session when the peer connection
      // closes anyway.
      await fetch(sessionUrl, { method: "DELETE" }).catch(() => undefined);
    }
    this.peerConnection?.close();
    this.peerConnection = null;
    if (this.ownedStream) {
      // Only a stream the publisher captured itself may be torn down; a
      // borrowed one still belongs to Piik's own share.
      this.stream?.getTracks().forEach((track) => track.stop());
      this.ownedStream = false;
    }
    this.stream = null;
  }
}
