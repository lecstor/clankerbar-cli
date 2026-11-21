package secureurl

import "regexp"

// urlUserinfoRe matches URL userinfo (`scheme://user:pass@`) inside free-form
// text. The userinfo class stops at path/query/fragment delimiters and quotes,
// so an `@` in a later path or query is never mistaken for a credential, and an
// scp-style address like `git@github.com:owner/repo.git` (no `://`) is left
// alone.
var urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^\s/?#'"<>]+)@`)

// RedactUserinfo removes every URL's userinfo from s, keeping the scheme, host
// and path so a log line or failure still names what it was doing. A string
// with no URL userinfo is returned byte for byte.
//
// It works on free-form text, not a single URL value: a failing command can
// quote a credential-bearing URL anywhere in its stderr, and an earlier
// credential-free URL must not stop a later credential from being redacted.
func RedactUserinfo(s string) string {
	return urlUserinfoRe.ReplaceAllString(s, "${1}redacted@")
}
