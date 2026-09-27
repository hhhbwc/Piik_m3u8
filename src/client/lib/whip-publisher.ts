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
const MAX_VIDEO_BITRATE_BPS = 5_000_000;

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

  get active(): boolean {
    return this.stream !== null;
  }

  /**
   * Captures the screen, negotiates WHIP, and hands the local stream to
   * `onPreview` once publishing. `onEnded` fires when the capture ends by
   * itself (the browser's "stop sharing" bar) — the caller resets its state;
   * no further stop() is needed.
   */
  async start(
    onPreview: (stream: MediaStream) => void,
    onEnded: () => void,
  ): Promise<void> {
    let stream: MediaStream;
    try {
      stream = await navigator.mediaDevices.getDisplayMedia({
        video: { frameRate: 30 },
        audio: true,
      });
    } catch {
      throw new ScreenPublishError("capture");
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

      // Keep a busy screen from starving the shared server link; advisory.
      for (const sender of peerConnection.getSenders()) {
        if (sender.track?.kind !== "video") {
          continue;
        }
        try {
          const parameters = sender.getParameters();
          if (parameters.encodings.length === 0) {
            parameters.encodings = [{}];
          }
          parameters.encodings[0].maxBitrate = MAX_VIDEO_BITRATE_BPS;
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
    this.stream?.getTracks().forEach((track) => track.stop());
    this.stream = null;
  }
}
