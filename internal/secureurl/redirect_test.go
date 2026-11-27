package secureurl

import (
	"net/http"
	"net/url"
	"testing"
)

// The exposure this guard closes is one Go's own redirect policy leaves open: an
// https -> http hop to the SAME host keeps Authorization attached.
func TestNoDowngradeRedirect(t *testing.T) {
	request := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return &http.Request{URL: u}
	}

	for _, raw := range []string{
		"https://clankerbar.com/mcp/x",
		"http://127.0.0.1:8787/api/x",
	} {
		if err := NoDowngradeRedirect(request(raw), nil); err != nil {
			t.Errorf("NoDowngradeRedirect(%q) = %v, want allowed", raw, err)
		}
	}

	for _, raw := range []string{
		"http://clankerbar.com/mcp/x",
		"http://attacker.example/mcp/x",
	} {
		if err := NoDowngradeRedirect(request(raw), nil); err == nil {
			t.Errorf("NoDowngradeRedirect(%q) = nil, want refused", raw)
		}
	}
}
