package events

import (
	"strings"
	"testing"
)

func TestRedactKnownPatterns(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nIBAAKCA\n-----END RSA PRIVATE KEY-----"
	cases := []struct {
		name string
		in   string
		want string
		hit  bool
	}{
		{
			name: "clk key",
			in:   "export CLANKERBAR_API_KEY=clk_" + strings.Repeat("a", 64),
			want: "export CLANKERBAR_API_KEY=[redacted]",
			hit:  true,
		},
		{
			name: "ghp key",
			in:   "git clone https://ghp_" + strings.Repeat("B", 36) + "@github.com/x/y",
			want: "git clone https://[redacted]@github.com/x/y",
			hit:  true,
		},
		{
			name: "github_pat key",
			in:   "token=github_pat_" + strings.Repeat("1", 40),
			want: "token=[redacted]",
			hit:  true,
		},
		{
			name: "anthropic key",
			in:   "ANTHROPIC_API_KEY=sk-ant-api03-" + strings.Repeat("x", 40),
			want: "ANTHROPIC_API_KEY=[redacted]",
			hit:  true,
		},
		{
			name: "aws key",
			in:   "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
			want: "AWS_ACCESS_KEY_ID=[redacted]",
			hit:  true,
		},
		{
			name: "pem block",
			in:   "cat <<EOF > key.pem\n" + pem + "\nEOF",
			want: "cat <<EOF > key.pem\n[redacted]\nEOF",
			hit:  true,
		},
		{
			name: "unterminated pem block",
			in:   "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n",
			want: "[redacted]",
			hit:  true,
		},
		{
			name: "ordinary prose",
			in:   "run the tests and read clk_test for the fixture",
			want: "run the tests and read clk_test for the fixture",
			hit:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, hit := Redact(tc.in)
			if got != tc.want || hit != tc.hit {
				t.Errorf("Redact() = %q, %v; want %q, %v", got, hit, tc.want, tc.hit)
			}
		})
	}
}

func TestRedactMultiplePatterns(t *testing.T) {
	in := "clk_" + strings.Repeat("a", 32) + " and AKIAIOSFODNN7EXAMPLE"
	got, hit := Redact(in)
	if !hit || strings.Contains(got, "AKIA") || strings.Contains(got, "clk_") {
		t.Fatalf("Redact() = %q, hit=%v; both patterns must be removed", got, hit)
	}
	if strings.Count(got, redactedMarker) != 2 {
		t.Errorf("Redact() = %q; want two %s markers", got, redactedMarker)
	}
}

func TestTruncateUTF8DoesNotSplitRunes(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
		cut  bool
	}{
		{"hello", 5, "hello", false},
		{"hello", 3, "hel", true},
		{"aé", 2, "a", true},
		{"é", 1, "", true},
		{"日本語", 4, "日", true},
		{"日本語", 9, "日本語", false},
	}
	for _, tc := range cases {
		got, cut := truncateUTF8(tc.in, tc.max)
		if got != tc.want || cut != tc.cut {
			t.Errorf("truncateUTF8(%q, %d) = %q, %v; want %q, %v", tc.in, tc.max, got, cut, tc.want, tc.cut)
		}
	}
}

func TestOneLine(t *testing.T) {
	got := oneLine("echo one\nrm -rf /tmp/x\r\nprintf 'a\rb'")
	want := "echo one rm -rf /tmp/x printf 'a b'"
	if got != want {
		t.Errorf("oneLine() = %q, want %q", got, want)
	}
}

func TestOneLineAfterRedactKeepsPEMIntact(t *testing.T) {
	// The ordering matters: a PEM block carries newlines, so flattening the
	// target first would leave the BEGIN line alone and the body exposed.
	in := "key:\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----"
	red, hit := Redact(in)
	if !hit || strings.Contains(red, "MIIE") {
		t.Fatalf("Redact() = %q; the PEM body survived", red)
	}
	if got := oneLine(red); strings.Contains(got, "\n") {
		t.Errorf("oneLine() left a newline: %q", got)
	}
}
