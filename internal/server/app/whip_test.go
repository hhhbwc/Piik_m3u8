package app

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The proxy translates /api/whip to the upstream WHIP endpoint, re-applies the
// configured credential query, and rewrites the session Location back to the
// client-facing prefix without credentials.
func TestWHIPProxyForwarding(t *testing.T) {
	var gotPath, gotQuery, gotMethod, gotBody, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		gotPath, gotQuery, gotMethod, gotBody =
			request.URL.Path, request.URL.RawQuery, request.Method, string(body)
		gotAuth = request.Header.Get("Authorization")
		if request.Method == http.MethodPost {
			writer.Header().Set("Location",
				"/screen/whip/23c53ddf-a5f7-4656-9c0a-654e6cd3f5e4")
			writer.Header().Set("Content-Type", "application/sdp")
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte("answer-sdp"))
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// Credentials come from the upstream URL's userinfo and reach MediaMTX as
	// a Basic header; the client's own header (and query) never does.
	parsed, err := url.Parse(upstream.URL + "/screen/whip")
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	parsed.User = url.UserPassword("obs", "secret")
	handler := newWHIPProxy(parsed)

	// POST /api/whip carries the offer and receives the rewritten Location.
	request := httptest.NewRequest(http.MethodPost, whipRoutePrefix+"?user=attacker&pass=x",
		strings.NewReader("offer-sdp"))
	request.Header.Set("Content-Type", "application/sdp")
	request.Header.Set("Authorization", "Basic YXR0YWNrZXI6eA==")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201", recorder.Code)
	}
	// The upstream keeps its own query, whatever the client appended.
	if gotPath != "/screen/whip" || gotBody != "offer-sdp" || gotQuery != "" {
		t.Fatalf("upstream saw %q %q %q", gotPath, gotQuery, gotBody)
	}
	if gotAuth != "Basic "+base64.StdEncoding.EncodeToString([]byte("obs:secret")) {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if location := recorder.Header().Get("Location"); location != whipRoutePrefix+"/23c53ddf-a5f7-4656-9c0a-654e6cd3f5e4" {
		t.Fatalf("Location = %q", location)
	}

	// DELETE of the session resource keeps the subpath and credential query.
	request = httptest.NewRequest(http.MethodDelete,
		whipRoutePrefix+"/23c53ddf-a5f7-4656-9c0a-654e6cd3f5e4", nil)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", recorder.Code)
	}
	if gotPath != "/screen/whip/23c53ddf-a5f7-4656-9c0a-654e6cd3f5e4" ||
		gotMethod != http.MethodDelete {
		t.Fatalf("upstream saw %q %q", gotPath, gotMethod)
	}
}

// An unreachable upstream answers 502 instead of hanging or panicking.
func TestWHIPProxyUnreachable(t *testing.T) {
	parsed, err := url.Parse("http://127.0.0.1:1/screen/whip?user=obs&pass=x")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	handler := newWHIPProxy(parsed)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, whipRoutePrefix,
		strings.NewReader("offer")))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}
}
