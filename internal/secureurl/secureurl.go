// Package secureurl holds one rule, in one place: where a bearer token may be
// sent.
//
// CLANKERBAR_API_KEY is ACCOUNT-scoped — it covers every project the operator is
// a member of. CLA-257 was the account key being sent to a host named by a file
// inside the workdir, over plain http. The destination is now pinned by config
// (see internal/config), and the scheme floor lives here because three packages
// need the same answer: config, when it decides what a config file may say, and
// backlog + plane, when a redirect tries to move a request that is already
// carrying the token.
package secureurl

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Origin returns the normalized scheme://host of raw if it is a place a bearer
// token may be sent, and an error naming the reason if it is not.
//
// The floor is TLS: `https` anywhere, or `http` only to loopback — where there is
// no network to eavesdrop on, and where a plane under local development lives.
// Anything else would put an account-scoped credential on the wire in cleartext.
func Origin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("%q has no scheme and host", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !IsLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("%q is plain http to a non-loopback host — the API key would go over the wire in cleartext; use https", raw)
		}
	default:
		return "", fmt.Errorf("%q has scheme %q; only https (or http to loopback) may carry the API key", raw, u.Scheme)
	}
	return scheme + "://" + normalizeHost(scheme, u), nil
}

// SameOrigin reports whether raw is on origin, which must already have come from
// Origin. A raw that is not a credential-safe URL is never the same origin as
// anything — the comparison fails closed rather than treating two unusable
// values as equal.
func SameOrigin(origin, raw string) bool {
	if origin == "" {
		return false
	}
	got, err := Origin(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(got, origin)
}

// normalizeHost drops the two spellings that make one host look like two: an
// explicitly written default port, and the trailing dot of a fully qualified
// name. Both are the same server, and refusing `https://clankerbar.com:443`
// against a pin of `https://clankerbar.com` would be a false alarm whose
// suggested remedy is to fix a file that is already pointing at the right place.
func normalizeHost(scheme string, u *url.URL) string {
	host, port := u.Hostname(), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	host = strings.TrimSuffix(host, ".")
	if port == "" {
		// An IPv6 literal keeps its brackets; Hostname() strips them.
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, port)
}

// IsLoopbackHost reports whether host is the local machine — 127.0.0.0/8, ::1, or
// the "localhost" name. Nothing is resolved: a DNS lookup here would let an
// attacker-controlled name masquerade as loopback for exactly as long as the
// lookup said so, and the point of this check is that it cannot be talked out of
// its answer.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
