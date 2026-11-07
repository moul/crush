package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRefusesBrowserRequests checks that the API, which only gnosh's own
// client talks to, refuses what a web page can send it through the user's
// browser when the server listens on TCP: a cross-site request, a request
// from a DNS-rebound name, which the browser marks same-origin, and a write
// carrying an Origin. A typed URL, such as the docs page, and the client
// itself still get through.
func TestRefusesBrowserRequests(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	srv.installHandler()
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	do := func(method, path string, header http.Header) int {
		t.Helper()
		var body *strings.Reader
		if method == http.MethodPost {
			body = strings.NewReader(`{"command":"shutdown"}`)
		} else {
			body = strings.NewReader("")
		}
		req, err := http.NewRequestWithContext(t.Context(), method, hs.URL+path, body)
		require.NoError(t, err)
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := hs.Client().Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}

	for _, header := range []http.Header{
		{"Sec-Fetch-Site": {"cross-site"}},
		{"Sec-Fetch-Site": {"same-site"}},
		{"Sec-Fetch-Site": {"same-origin"}},
		{"Origin": {"http://evil.example"}},
	} {
		require.Equal(t, http.StatusForbidden, do(http.MethodGet, "/v1/health", header), header)
	}
	require.Equal(t, http.StatusForbidden, do(http.MethodPost, "/v1/control", http.Header{
		"Content-Type": {"text/plain"},
		"Origin":       {"null"},
	}))

	require.Equal(t, http.StatusOK, do(http.MethodGet, "/v1/health", nil))
	require.Equal(t, http.StatusOK, do(http.MethodGet, "/v1/docs/", http.Header{
		"Sec-Fetch-Site": {"none"},
	}))
}
