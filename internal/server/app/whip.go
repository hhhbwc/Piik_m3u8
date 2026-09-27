package app

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// whipRoutePrefix is the client-facing WHIP signaling prefix. POST carries the
// offer SDP and is answered with 201 plus a Location header naming the session
// resource; DELETE tears that session down; PATCH and OPTIONS exist for
// trickle-ICE clients and are forwarded untouched.
const whipRoutePrefix = "/api/whip"

// isWHIPPath reports whether the request path belongs to the WHIP proxy.
func isWHIPPath(path string) bool {
	return path == whipRoutePrefix || strings.HasPrefix(path, whipRoutePrefix+"/")
}

// newWHIPProxy forwards WHIP signaling to the MediaMTX WebRTC ingest listening
// on the same host. The upstream URL's userinfo carries the publish
// credentials, which the proxy turns into the HTTP Basic header MediaMTX
// requires (it ignores query credentials), so the browser never sees them.
// The same-origin route also avoids CORS entirely: the client talks to its own
// origin and ICE then flows directly to the ingest endpoint's public UDP port.
//
// The proxy forwards no client query and no client Authorization header: the
// configured credentials are the only ones honored, and the configured query
// is re-applied on every hop. Location responses are translated back to the
// client-facing prefix, stripped of scheme, host and query.
func newWHIPProxy(upstream *url.URL) http.Handler {
	basePath := strings.TrimSuffix(upstream.Path, "/")
	baseQuery := upstream.RawQuery
	user, password := "", ""
	if upstream.User != nil {
		user = upstream.User.Username()
		password, _ = upstream.User.Password()
	}
	outgoingAuth := user != "" || password != ""

	return &httputil.ReverseProxy{
		Director: func(request *http.Request) {
			sub := strings.TrimPrefix(request.URL.Path, whipRoutePrefix)
			request.URL.Scheme = upstream.Scheme
			request.URL.Host = upstream.Host
			request.URL.User = nil
			request.URL.Path = basePath + sub
			request.URL.RawQuery = baseQuery
			request.Host = upstream.Host
			request.Header.Del("Authorization")
			if outgoingAuth {
				request.SetBasicAuth(user, password)
			}
		},
		ModifyResponse: func(response *http.Response) error {
			location := response.Header.Get("Location")
			if location == "" {
				return nil
			}
			parsed, err := url.Parse(location)
			if err != nil {
				return nil
			}
			if parsed.Path != basePath && !strings.HasPrefix(parsed.Path, basePath+"/") {
				return nil
			}
			parsed.Scheme = ""
			parsed.Host = ""
			parsed.Path = whipRoutePrefix + strings.TrimPrefix(parsed.Path, basePath)
			parsed.RawQuery = ""
			parsed.Fragment = ""
			response.Header.Set("Location", parsed.String())
			return nil
		},
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, _ error) {
			sendJSON(writer, http.StatusBadGateway, errorBody{"Stream ingest unreachable"})
		},
	}
}
