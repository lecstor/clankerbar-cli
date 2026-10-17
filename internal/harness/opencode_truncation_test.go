package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// truncatedClaimLine is a tool_use event in the shape 2.0.32 emits for an MCP
// result above 51,200 bytes (recorded by the CLA-675 review): the output is a
// pointer, and metadata.metadata carries truncated/outputPath.
func truncatedClaimLine(path string) string {
	ev := map[string]any{
		"type": "tool_use", "sessionID": "ses_x",
		"part": map[string]any{
			"partID": "prt_1", "type": "tool", "id": "call_0", "tool": "clankerbar_claim_task",
			"state": map[string]any{
				"status": "completed",
				"input":  map[string]any{"taskId": "T-9"},
				"output": "[showing 0 lines of 1; full output saved to " + path + "]",
				"title":  "clankerbar_claim_task",
				"metadata": map[string]any{
					"metadata": map[string]any{"truncated": true, "outputPath": path},
					"content":  []any{map[string]any{"type": "text", "text": "[showing 0 lines of 1; full output saved to " + path + "]"}},
				},
			},
		},
	}
	b, _ := json.Marshal(ev)
	return string(b)
}

const bigClaimPayload = `{"task":{"id":"T-9","ref":"CLA-1"},"run":{"id":"run-1"},"decisions":[]}`

// parseWithData runs the parser for a session whose data home is data.
func parseWithData(t *testing.T, data, line string) (Result, string) {
	t.Helper()
	var console bytes.Buffer
	res := opencodeParsedFrom(Invocation{Env: []string{"XDG_DATA_HOME=" + data}}, &console, line)
	return res, console.String()
}

func TestOpencodeRehydratesATruncatedClaim(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "opencode", "tool-output")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tool_14be859510016lUEsFzsv0H3FT")
	if err := os.WriteFile(path, []byte(bigClaimPayload), 0o644); err != nil {
		t.Fatal(err)
	}
	res, console := parseWithData(t, data, truncatedClaimLine(path))
	if !res.Claim.Held() || res.Claim.TaskID != "T-9" || res.Claim.RunID != "run-1" {
		t.Fatalf("Claim = %+v; the truncated claim was not rehydrated. console: %s", res.Claim, console)
	}
}

// Every guard on the path fails safe: no claim, and a diagnostic naming why.
func TestOpencodeTruncatedClaimFailsSafe(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "opencode", "tool-output")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	outside := filepath.Join(elsewhere, "tool_forged")
	if err := os.WriteFile(outside, []byte(bigClaimPayload), 0o644); err != nil {
		t.Fatal(err)
	}
	badName := filepath.Join(dir, "claim.json")
	if err := os.WriteFile(badName, []byte(bigClaimPayload), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "tool_link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"outside the tool-output dir": outside,
		"not a tool_ file":            badName,
		"symlink":                     link,
		"missing":                     filepath.Join(dir, "tool_gone"),
		"relative":                    "tool_rel",
		"dot-dot escape":              filepath.Join(dir, "..", "tool_up"),
	} {
		t.Run(name, func(t *testing.T) {
			res, console := parseWithData(t, data, truncatedClaimLine(path))
			if res.Claim.Held() {
				t.Errorf("Claim = %+v from a path that must be refused", res.Claim)
			}
			if !strings.Contains(console, "truncated by opencode and could not be read back") {
				t.Errorf("no diagnostic; console: %s", console)
			}
		})
	}
}

// A result that is not truncated is used as it arrived, even if its text looks
// like the pointer: only opencode's own metadata triggers a read.
func TestOpencodeUntruncatedOutputIsNotRehydrated(t *testing.T) {
	line := strings.Replace(truncatedClaimLine("/etc/hosts"), `"truncated":true`, `"truncated":false`, 1)
	res, _ := parseWithData(t, t.TempDir(), line)
	if res.Claim.Held() {
		t.Errorf("Claim = %+v", res.Claim)
	}
}

func TestOpencodeToolOutputDir(t *testing.T) {
	if got := opencodeToolOutputDir(Invocation{Env: []string{"XDG_DATA_HOME=/d"}}); got != "/d/opencode/tool-output" {
		t.Errorf("got %q", got)
	}
	t.Setenv("XDG_DATA_HOME", "")
	if got := opencodeToolOutputDir(Invocation{Env: []string{"HOME=/h"}}); got != "/h/.local/share/opencode/tool-output" {
		t.Errorf("got %q", got)
	}
}

// Only the typed arm can stop these: no status code, no fatal word, and usage
// reported, so without `"type":"provider.(auth|no-route)"` the CLA-381 heuristic
// would read each as an unclassified post-usage failure and retry it.
func TestOpencodeTypedFatalArmAloneStops(t *testing.T) {
	for _, line := range []string{
		`{"type":"error","sessionID":"ses_x","error":{"type":"provider.auth","message":"credential rejected"}}`,
		`{"type":"error","sessionID":"ses_x","error":{"type":"provider.no-route","message":"Model gone: x/y"}}`,
	} {
		res := Result{ExitCode: 1, UsageReported: true, Stdout: line}
		if (opencode{}).IsTransient(res) {
			t.Errorf("IsTransient = true for %s; the typed fatal arm did not fire", line)
		}
	}
}
