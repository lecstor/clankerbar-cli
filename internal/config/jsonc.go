package config

import "encoding/json"

// JSONC - JSON with comments and trailing commas - is what opencode's own config
// files are written in, and `opencode.jsonc` is the conventional name for the
// global one. Go's encoding/json refuses both extensions, so a strict parse of a
// perfectly ordinary opencode config fails, and the ambient-config check
// (opencodeambient.go) would have reported "nothing to see here" for exactly the
// file that caused CLA-441.
//
// This tolerance is confined to files opencode DISCOVERS on its own. The files
// the driver NAMES - mcp_config_path and its per-harness and per-project
// variants - are still parsed strictly by readMCPFile, and deliberately: those
// feed security gates that fail closed, and widening what they accept would
// widen what an untrusted workdir file can say without being refused. Two
// parsers is the price of not moving that line.

// parseJSONCInto parses a JSONC document into the mcpFile model.
func parseJSONCInto(data []byte) (mcpFile, error) {
	var f mcpFile
	err := json.Unmarshal(stripJSONC(data), &f)
	return f, err
}

// stripJSONC rewrites a JSONC document into plain JSON: `//` and `/* */`
// comments become spaces, and a comma directly before `}` or `]` is dropped.
//
// It is STRING-AWARE, which is the whole difficulty - a `//` inside a URL is not
// a comment, and every clankerbar server entry in one of these files contains
// exactly that ("https://clankerbar.com/mcp/<slug>"). Comment bytes are replaced
// with spaces rather than removed so byte offsets in a json.Unmarshal error still
// point at the right place in the operator's file.
func stripJSONC(data []byte) []byte {
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
// by `}` or `]`. Run after stripJSONC's comment pass, so a comma separated from
// its closing brace by a comment is caught too.
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
				// Drop the comma, keep the whitespace: same reason stripJSONC
				// pads rather than deletes.
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
