package jsonc

import (
	"encoding/json"
	"testing"
)

// Strip is string-aware: every clankerbar URL in these files contains "//", and
// a quoted "/* */" is data rather than a comment. This is the property that
// makes the stripper usable at all rather than a regex.
func TestStripLeavesStringsAlone(t *testing.T) {
	in := []byte(`{"url":"https://clankerbar.com/mcp/x","q":"a /* b */ c"} // tail`)
	var got map[string]string
	if err := json.Unmarshal(Strip(in), &got); err != nil {
		t.Fatalf("Strip output is not JSON: %v", err)
	}
	if got["url"] != "https://clankerbar.com/mcp/x" {
		t.Errorf("url = %q, want the URL untouched", got["url"])
	}
	if got["q"] != "a /* b */ c" {
		t.Errorf("q = %q, want the quoted comment-looking string untouched", got["q"])
	}
}

func TestStripCommentsAndTrailingCommas(t *testing.T) {
	in := []byte(`{
  // line comment
  "a": 1, /* block
  comment */
  "b": [1, 2,],
}`)
	var got map[string]any
	if err := json.Unmarshal(Strip(in), &got); err != nil {
		t.Fatalf("Strip output is not JSON: %v", err)
	}
	if got["a"] != float64(1) {
		t.Errorf("a = %v, want 1", got["a"])
	}
	if b, ok := got["b"].([]any); !ok || len(b) != 2 {
		t.Errorf("b = %v, want the two-element array", got["b"])
	}
}

// An unterminated block comment must not panic or loop: the rest of the file is
// blanked, and the decoder reports the malformed document.
func TestStripUnterminatedBlockComment(t *testing.T) {
	out := Strip([]byte(`{"a":1} /* never closed`))
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("the valid prefix was lost: %v", err)
	}
}
