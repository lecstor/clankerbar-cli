package secureurl

import (
	"strings"
	"testing"
)

func TestRedactUserinfo(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "github x-access-token in git stderr",
			in:   "fatal: unable to access 'https://x-access-token:gho_secret123@github.com/lecstor/repo.git/': Failed to connect",
			want: "fatal: unable to access 'https://redacted@github.com/lecstor/repo.git/': Failed to connect",
		},
		{
			name: "general user:pass and password containing @",
			in:   "error fetching http://alice:p@ss@git.example.com/org/repo.git and https://bob:pw@mirror.local/r",
			want: "error fetching http://redacted@git.example.com/org/repo.git and https://redacted@mirror.local/r",
		},
		{
			name: "preserves scp ssh and @ inside path",
			in:   "git@github-personal:lecstor/repo.git https://github.com/org/email@example.com",
			want: "git@github-personal:lecstor/repo.git https://github.com/org/email@example.com",
		},
		{
			name: "credential-free URL before a credential-bearing one is not a shield",
			in:   "GET http://mirror.local/status then https://user:token@git.example.com/org/repo",
			want: "GET http://mirror.local/status then https://redacted@git.example.com/org/repo",
		},
		{
			name: "uppercase scheme and ssh URL",
			in:   "HTTPS://user:pw@git.example.com/r ssh://git:pw@host/r",
			want: "HTTPS://redacted@git.example.com/r ssh://redacted@host/r",
		},
		{
			name: "password containing an unencoded apostrophe",
			in:   "https://user:pa'ss@git.example.com/mirror/",
			want: "https://redacted@git.example.com/mirror/",
		},
		{
			name: "password containing an unencoded apostrophe in quoted git stderr",
			in:   "fatal: repository 'https://user:pa'ss@git.example.com/mirror/' not found",
			want: "fatal: repository 'https://redacted@git.example.com/mirror/' not found",
		},
		{
			name: "no userinfo is unchanged",
			in:   "https://git.example.com/mirror/ file:///srv/mirrors/ plain text",
			want: "https://git.example.com/mirror/ file:///srv/mirrors/ plain text",
		},
		{
			name: "an @ in query or fragment is not userinfo",
			in:   "https://git.example.com/r?email=a@b https://git.example.com/r#x@y",
			want: "https://git.example.com/r?email=a@b https://git.example.com/r#x@y",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RedactUserinfo(c.in); got != c.want {
				t.Errorf("RedactUserinfo(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The redactor is the last line of defence before text reaches a log, so pin
// that no password survives in any shape it is likely to arrive in.
func TestRedactUserinfoLeavesNoCredential(t *testing.T) {
	const secret = "gho_super_secret_token"
	for _, in := range []string{
		"https://x-access-token:" + secret + "@github.com/org/repo.git",
		"fatal: unable to access 'https://x-access-token:" + secret + "@github.com/': 502",
	} {
		if got := RedactUserinfo(in); strings.Contains(got, secret) {
			t.Errorf("RedactUserinfo(%q) leaked the secret: %q", in, got)
		}
	}
}
