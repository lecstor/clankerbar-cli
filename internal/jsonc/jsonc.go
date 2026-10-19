// Package jsonc strips the JSONC extensions opencode writes its own config
// files in: `//` and `/* */` comments, and a comma directly before a closing
// brace or bracket.
//
// It is deliberately a STRIPPER, not a parser: callers hand the result to
// encoding/json, so the decoded values, the error text and the byte offsets in
// that error are Go's own. Two consumers need it for the same reason — the
// configuration package's ambient-config audit and the harness adapter's MCP
// preflight both read files opencode discovered on its own, which are commonly
// `opencode.jsonc` and routinely carry comments.
//
// The tolerance is confined to those discovered files. Files the driver NAMES
// are parsed strictly, because they feed fail-closed security gates.
package jsonc

// Strip rewrites a JSONC document into plain JSON: comments become spaces, and
// a comma directly before `}` or `]` is dropped.
//
// It is STRING-AWARE, which is the whole difficulty - a `//` inside a URL is not
// a comment, and every clankerbar server entry in one of these files contains
// exactly that ("https://clankerbar.com/mcp/<slug>"). Comment bytes are replaced
// with spaces rather than removed so byte offsets in a json.Unmarshal error still
// point at the right place in the operator's file.
func Strip(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		ch := data[i]
		if inString {
			out = append(out, ch)
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch {
		case ch == '"':
			inString = true
			out = append(out, ch)
		case ch == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				out = append(out, ' ')
				i++
			}
			// The newline itself survives: line numbers in a parse error stay true.
			if i < len(data) {
				out = append(out, data[i])
			}
		case ch == '/' && i+1 < len(data) && data[i+1] == '*':
			for i < len(data) && !(data[i] == '*' && i+1 < len(data) && data[i+1] == '/') {
				if data[i] == '\n' {
					out = append(out, '\n')
				} else {
					out = append(out, ' ')
				}
				i++
			}
			// The closing "*/", or the rest of an unterminated block comment.
			if i < len(data) {
				out = append(out, ' ', ' ')
				i++
			}
		default:
			out = append(out, ch)
		}
	}
	return dropTrailingCommas(out)
}

// dropTrailingCommas removes a comma that is followed - across whitespace only -
// by `}` or `]`. Run after Strip's comment pass, so a comma separated from its
// closing brace by a comment is caught too.
func dropTrailingCommas(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		ch := data[i]
		if inString {
			out = append(out, ch)
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}
		if ch == ',' {
			j := i + 1
			for j < len(data) && isJSONSpace(data[j]) {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				// Drop the comma, keep the whitespace: same reason Strip pads
				// rather than deletes.
				out = append(out, ' ')
				continue
			}
		}
		out = append(out, ch)
	}
	return out
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
