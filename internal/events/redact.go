package events

import (
	"regexp"
	"unicode/utf8"
)

// redactedMarker is what a matched secret span becomes. It is deliberately
// unmistakable in a stored transcript: a reader must be able to see that
// something was removed rather than mistake the gap for the agent's own words.
const redactedMarker = "[redacted]"

// The known key patterns from the channel's design (decision 6040fb25). The
// lengths are deliberately generous at the low end: a false positive costs a
// reader one [redacted] in a command line, while a false negative puts a live
// credential in a transcript that is kept for weeks. Matches are replaced
// whole, so the pattern's prefix and body never survive separately.
var (
	// clk_ is the plane's own API key ("clk_" + a long random body). The 8-char
	// floor keeps ordinary prose like "clk_test" from being mangled while still
	// covering a real key many times over.
	clkKeyRe = regexp.MustCompile(`clk_[A-Za-z0-9]{8,}`)

	// GitHub tokens: classic personal access tokens (ghp_) and fine-grained ones
	// (github_pat_).
	ghpKeyRe    = regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`)
	githubPatRe = regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)

	// Anthropic API keys.
	anthropicKeyRe = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`)

	// AWS access key ids. The 16-char body is AWS's fixed shape, and the AKIA
	// prefix is uppercase, so this does not fire on lowercase prose.
	awsKeyRe = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)

	// PEM private-key blocks, terminated form first: the block includes its
	// BEGIN/END armour and every body line, across newlines. The unterminated
	// second pattern is the best-effort case — a key pasted without its END line
	// must not survive just because the file was cut off.
	pemTerminatedRe = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	pemOpenRe       = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*$`)

	redactors = []*regexp.Regexp{
		pemTerminatedRe,
		pemOpenRe,
		clkKeyRe,
		ghpKeyRe,
		githubPatRe,
		anthropicKeyRe,
		awsKeyRe,
	}
)

// Redact replaces every known key pattern in s and reports whether anything
// matched. It runs over the FULL string before truncation, so a secret that
// straddles the cap boundary is not half-missed (the design note's ordering).
//
// Best effort, not a guarantee: an unrecognised credential shape passes through,
// which is why the tiers and the project's dials exist.
func Redact(s string) (string, bool) {
	redacted := false
	for _, re := range redactors {
		if !re.MatchString(s) {
			continue
		}
		s = re.ReplaceAllString(s, redactedMarker)
		redacted = true
	}
	return s, redacted
}

// truncateUTF8 cuts s to at most maxBytes without splitting a UTF-8 code point.
// It reports whether it cut. The design caps are BYTES (the plane validates
// them), and a byte-oriented cut through a multi-byte rune would leave invalid
// UTF-8 that Go's JSON encoder then rewrites as U+FFFD, silently growing the
// string past the cap it was just cut to.
func truncateUTF8(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}

// newlineRe matches any newline spelling. Compiled once: oneLine runs on every
// tool call of every session.
var newlineRe = regexp.MustCompile(`\r\n|\r|\n`)

// oneLine turns a multi-line target into a single line, as the schema requires
// ("ONE line"). Newlines become spaces rather than being deleted, so a heredoc
// or a shell pipeline keeps its word boundaries.
func oneLine(s string) string {
	return newlineRe.ReplaceAllString(s, " ")
}
