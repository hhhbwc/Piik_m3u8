package app

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// serveHLS exposes a static HLS tree as /live/<name> so external players
// (VRChat's AVPro, Safari, hls.js) can read playlists written by a stream
// bridge such as MediaMTX. The route is enabled by config.HLSRoot and maps
// /live/<name> to <HLSRoot>/live/<name>.
//
// Only .m3u8 playlists and .ts segments are served: anything else 404s, which
// also disables the directory listings http.FileServer would render. Playlists
// are no-store because their contents rotate with every segment; segments are
// immutable once written and get a short shared lifetime.
func serveHLS(writer http.ResponseWriter, request *http.Request, root string, routePath string) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		sendJSON(writer, http.StatusMethodNotAllowed, errorBody{"Method not allowed"})
		return
	}
	name := strings.TrimPrefix(routePath, "/live/")
	if name == "" || strings.HasSuffix(name, "/") {
		sendJSON(writer, http.StatusNotFound, errorBody{"Not found"})
		return
	}
	ext := strings.ToLower(path.Ext(name))
	if ext != ".m3u8" && ext != ".ts" {
		sendJSON(writer, http.StatusNotFound, errorBody{"Not found"})
		return
	}
	full := filepath.Join(root, "live", filepath.FromSlash(name))
	// filepath.Join cleans, so a traversal attempt either normalizes back
	// under root (harmless) or fails the prefix check below.
	if !strings.HasPrefix(full, filepath.Join(root, "live")+string(os.PathSeparator)) {
		sendJSON(writer, http.StatusNotFound, errorBody{"Not found"})
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		sendJSON(writer, http.StatusNotFound, errorBody{"Not found"})
		return
	}
	if ext == ".m3u8" {
		writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		writer.Header().Set("Cache-Control", "no-store")
	} else {
		writer.Header().Set("Content-Type", "video/mp2t")
		writer.Header().Set("Cache-Control", "public, max-age=60")
	}
	// ServeFile re-derives Content-Type only when it is unset, so the explicit
	// values above always win. It rejects a request path that does not match
	// the file being served (": invalid"), which cannot happen here because
	// the request path is never passed to it.
	http.ServeFile(writer, request, full)
}
