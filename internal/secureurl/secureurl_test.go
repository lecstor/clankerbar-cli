package secureurl

import "testing"

// The vectors an adversarial review reached for. Each is correct today; pinning
// them means a refactor cannot silently reopen one — swapping u.Hostname() for
// u.Host would hand the first case to an attacker.
func TestIsLoopbackHostVectors(t *testing.T) {
	allowed := []string{
		"http://127.0.0.1/x",
		"http://localhost/x",
		"http://localhost./x",
		"http://[::1]/x",
		"http://[::ffff:127.0.0.1]/x", // an IPv4-mapped loopback really is loopback
		"http://127.0.0.2/x",          // all of 127.0.0.0/8
	}
	for _, raw := range allowed {
		if _, err := Origin(raw); err != nil {
			t.Errorf("Origin(%q) = %v, want allowed", raw, err)
		}
	}

	refused := []string{
		"http://127.0.0.1@attacker.example/", // userinfo, not a host
		"http://localhost.attacker.example/",
		"http://127.0.0.1.attacker.example/",
		"http://0x7f.0.0.1/",
		"http://0/",
		"http://attacker.example/",
	}
	for _, raw := range refused {
		if got, err := Origin(raw); err == nil {
			t.Errorf("Origin(%q) = %q, want refused", raw, got)
		}
	}
}

func TestOriginNormalizesEquivalentSpellings(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://clankerbar.com:443/x", "https://clankerbar.com"},
		{"https://clankerbar.com./x", "https://clankerbar.com"},
		{"http://localhost:80/x", "http://localhost"},
		{"https://clankerbar.com:8443/x", "https://clankerbar.com:8443"},
		{"http://[::1]:8787/x", "http://[::1]:8787"},
		{"http://[::1]/x", "http://[::1]"},
	} {
		got, err := Origin(tc.raw)
		if err != nil {
			t.Errorf("Origin(%q) = %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Origin(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestSameOriginFailsClosed(t *testing.T) {
	if SameOrigin("", "https://clankerbar.com") {
		t.Error("SameOrigin with no pinned origin must be false")
	}
	if SameOrigin("https://clankerbar.com", "not a url at all") {
		t.Error("SameOrigin with an unusable candidate must be false")
	}
	if !SameOrigin("https://clankerbar.com", "https://clankerbar.com:443/mcp") {
		t.Error("SameOrigin must accept an equivalent spelling")
	}
}
