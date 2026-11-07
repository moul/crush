package server

import "net/http"

// browserGuard refuses what a web page can make the user's browser send to
// the API. Only gnosh's own client talks to it, and that client sends
// neither header below, while a browser sends Sec-Fetch-Site on every
// request and an Origin on every write. Over TCP nothing else stands between
// a page and the API: a cross-site POST with a text/plain body, the type the
// client itself sends, needs no preflight, and a name rebound to 127.0.0.1
// makes the page same-origin with the server. So only a typed URL or a
// bookmark, Sec-Fetch-Site none, gets through from a browser, as the docs
// page needs.
func browserGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "none" {
			jsonError(w, http.StatusForbidden, "requests from a web page are not allowed")
			return
		}
		if r.Header.Get("Origin") != "" {
			jsonError(w, http.StatusForbidden, "requests with an Origin are not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}
