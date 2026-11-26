package config

import (
	"context"
	"net/netip"
	"net/url"
	"strings"
)

// localSuffixes are the name endings that mark a host as being on the
// user's own network.
var localSuffixes = []string{".localhost", ".local", ".lan", ".internal", ".home.arpa"}

// overlayNet is the shared address range that private overlay networks such as
// Tailscale hand out.
var overlayNet = netip.MustParsePrefix("100.64.0.0/10")

// IsLocal reports whether a host is on the user's own machine or network:
// localhost, a loopback, unspecified, private or link-local address, an
// address in 100.64.0.0/10, a host name of one label, or a name under
// .localhost, .local, .lan, .internal or .home.arpa. The unspecified
// addresses 0.0.0.0 and :: belong there because a server that binds every
// interface prints one of them as its address (llama.cpp's server: "main:
// server is listening on http://0.0.0.0:8080"), and reaching it there
// reaches this machine. A model server there is given as long as it takes
// to start answering.
func IsLocal(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i] // an IPv6 zone
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		return addr.IsLoopback() || addr.IsUnspecified() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || overlayNet.Contains(addr)
	}
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range localSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// apiRoot completes a configured address into the root of the API. For a
// type that lives on the user's own machine the root ends in /v1 whether or
// not the address did; for the others /v1 is added only to an address with
// no path at all.
func apiRoot(given string, t *providerType) string {
	u, err := url.Parse(given)
	if err != nil {
		return given
	}
	path := strings.TrimRight(u.Path, "/")
	switch {
	case t.v1 == "always" && !strings.HasSuffix(path, "/v1"):
		path += "/v1"
	case t.v1 == "bare" && path == "":
		path = "/v1"
	}
	u.Path, u.RawPath = path, ""
	return u.String()
}

// SelectEndpoint picks the address of a provider to use: the first, in the
// order they were configured, that accepts a connection. dial is given each
// address's URL in turn and returns nil when something answered there; it
// should open a connection and close it, and send nothing. The second
// result says, for each address tried, why it was passed over. With one
// address configured, that address is returned without being tried: a
// server that is down is found out when it is used.
func SelectEndpoint(ctx context.Context, p Provider, dial func(ctx context.Context, url string) error) (Endpoint, []string, bool) {
	if len(p.Endpoints) == 0 {
		return Endpoint{}, nil, false
	}
	if len(p.Endpoints) == 1 || dial == nil {
		return p.Endpoints[0], nil, true
	}
	var passed []string
	for _, e := range p.Endpoints {
		err := dial(ctx, e.URL)
		if err == nil {
			return e, passed, true
		}
		passed = append(passed, e.Display+" did not answer: "+err.Error())
		if ctx.Err() != nil {
			break
		}
	}
	return Endpoint{}, passed, false
}
