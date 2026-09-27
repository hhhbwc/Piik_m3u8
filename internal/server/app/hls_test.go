package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/TNTcraftHIM/Piik/internal/server/config"
)

// hlsFixture writes a minimal MediaMTX-shaped tree: a live/ subdirectory with
// one playlist and one segment, plus a file outside live/ that must never be
// reachable.
func hlsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	live := filepath.Join(root, "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(live, "index.m3u8"):       "#EXTM3U\n",
		filepath.Join(live, "main_stream.m3u8"): "#EXTM3U\nseg.ts\n",
		filepath.Join(live, "seg1.ts"):          "\x47TS",
		filepath.Join(root, "secret.txt"):       "outside live",
	}
	for path, data := range files {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestHLSServing(t *testing.T) {
	root := hlsFixture(t)
	handler := func(method, target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, target, nil)
		recorder := httptest.NewRecorder()
		serveHLS(recorder, request, root, request.URL.Path)
		return recorder
	}

	t.Run("playlist is served with no-store and the Apple MIME type", func(t *testing.T) {
		recorder := handler(http.MethodGet, "/live/main_stream.m3u8")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
		if recorder.Body.String() != "#EXTM3U\nseg.ts\n" {
			t.Fatalf("body = %q", recorder.Body.String())
		}
	})

	t.Run("segment is served as MPEG-TS with a bounded lifetime", func(t *testing.T) {
		recorder := handler(http.MethodGet, "/live/seg1.ts")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != "video/mp2t" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "public, max-age=60" {
			t.Fatalf("Cache-Control = %q", got)
		}
	})

	t.Run("HEAD works", func(t *testing.T) {
		recorder := handler(http.MethodHead, "/live/seg1.ts")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
	})

	t.Run("POST is a 405 with Allow", func(t *testing.T) {
		recorder := handler(http.MethodPost, "/live/seg1.ts")
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", recorder.Code)
		}
		if got := recorder.Header().Get("Allow"); got != "GET, HEAD" {
			t.Fatalf("Allow = %q", got)
		}
	})

	t.Run("the directory root, traversal and foreign extensions 404", func(t *testing.T) {
		for _, target := range []string{
			"/live/",
			"/live",
			"/live/../secret.txt",
			"/live/..%2Fsecret.txt",
			"/live/index.html",
			"/live/missing.ts",
		} {
			recorder := handler(http.MethodGet, target)
			if recorder.Code != http.StatusNotFound {
				t.Errorf("%s: status = %d, want 404", target, recorder.Code)
			}
		}
	})

	t.Run("the file outside live/ is unreachable", func(t *testing.T) {
		recorder := handler(http.MethodGet, "/live/../../secret.txt")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", recorder.Code)
		}
	})
}

func TestHLSRouteDisabledWithoutConfig(t *testing.T) {
	// An empty HLSRoot must leave /live/ to the frontend fallthrough; the
	// route gate in route() checks it before calling serveHLS. This test
	// pins the contract that an unset root is the off switch.
	loaded, err := config.Load(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.HLSRoot != "" {
		t.Fatalf("HLSRoot = %q, want empty", loaded.HLSRoot)
	}
}

func TestHLSConfigParsing(t *testing.T) {
	t.Run("relative paths are rejected", func(t *testing.T) {
		if _, err := config.Load(map[string]string{"HLS_SERVE_DIR": "relative/path"}); err == nil {
			t.Fatal("expected an error for a relative HLS_SERVE_DIR")
		}
	})
	t.Run("absolute paths are accepted", func(t *testing.T) {
		// t.TempDir is absolute on every host OS; a hardcoded "/srv/..."
		// would not be absolute on Windows.
		absolute := filepath.Join(t.TempDir(), "vrchat-hls")
		loaded, err := config.Load(map[string]string{"HLS_SERVE_DIR": absolute})
		if err != nil {
			t.Fatal(err)
		}
		if loaded.HLSRoot != absolute {
			t.Fatalf("HLSRoot = %q, want %q", loaded.HLSRoot, absolute)
		}
	})
}
