package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exportDoc is a session export as 2.0.32 prints it, trimmed to the fields the
// adapter reads. Recorded shape: info.cost/info.tokens are the session totals
// (title generation included), and each assistant message carries its own
// finish, cost and tokens.
func exportDoc(t *testing.T, infoTokens int, infoCost float64, msgs ...opencodeExportMessage) opencodeExport {
	t.Helper()
	var doc opencodeExport
	tk := opencodeExportTokens{Input: infoTokens}
	doc.Info.Tokens = &tk
	doc.Info.Cost = &infoCost
	doc.Messages = msgs
	return doc
}

func assistantMsg(created int64, finish string, input, output, reasoning int, cost float64) opencodeExportMessage {
	m := opencodeExportMessage{Type: "assistant", Finish: finish, Cost: cost,
		Tokens: &opencodeExportTokens{Input: input, Output: output, Reasoning: reasoning}}
	m.Time.Created = created
	return m
}

// A fresh session is charged its messages plus the overhead outside them (title
// generation), and the final assistant message supplies the finish reason the
// stream never carried.
func TestApplyOpencodeExportChargesTheCreatingRunWithTheOverhead(t *testing.T) {
	start := time.UnixMilli(1000)
	doc := exportDoc(t, 3*18511, 0.3,
		assistantMsg(1100, "tool-calls", 18511, 2, 0, 0.1),
		assistantMsg(1200, "stop", 18511, 2, 0, 0.1))
	doc.Info.Tokens.Output = 6

	var res Result
	applyOpencodeExport(&res, doc, start, false)
	if res.Tokens != 3*18511+6 {
		t.Errorf("Tokens = %d, want the session total %d", res.Tokens, 3*18511+6)
	}
	if res.CostUSD < 0.2999 || res.CostUSD > 0.3001 {
		t.Errorf("CostUSD = %v, want 0.3", res.CostUSD)
	}
	if !res.UsageReported {
		t.Error("UsageReported = false")
	}
	if res.Raw[FinishReasonKey] != "stop" {
		t.Errorf("finish_reason = %v, want stop", res.Raw[FinishReasonKey])
	}
	if res.Raw[opencodeUsageSourceKey] != opencodeUsageExport {
		t.Errorf("usage_source = %v", res.Raw[opencodeUsageSourceKey])
	}
}

// A resumed run is charged only the messages created after it started: the
// export is cumulative, and the earlier runs were already charged.
func TestApplyOpencodeExportChargesAResumeOnlyItsOwnMessages(t *testing.T) {
	doc := exportDoc(t, 3*100+50, 0.35,
		assistantMsg(1100, "stop", 100, 0, 0, 0.1),
		assistantMsg(2100, "tool-calls", 100, 0, 0, 0.1),
		assistantMsg(2200, "stop", 100, 0, 0, 0.1))
	var res Result
	applyOpencodeExport(&res, doc, time.UnixMilli(2000), true)
	if res.Tokens != 200 {
		t.Errorf("Tokens = %d, want 200 (two messages after the resume started, no overhead)", res.Tokens)
	}
	if res.CostUSD < 0.1999 || res.CostUSD > 0.2001 {
		t.Errorf("CostUSD = %v, want 0.2", res.CostUSD)
	}
}

// The final-step markers the stream used to supply are recomputed from the
// export's last assistant message.
func TestApplyOpencodeExportRecomputesTheFinalStepMarkers(t *testing.T) {
	stall := exportDoc(t, 0, 0, assistantMsg(1100, "length", 500, 0, 32000, 0.02))
	var res Result
	applyOpencodeExport(&res, stall, time.UnixMilli(1000), false)
	if !(opencode{}).OutputCapNoOutput(res) || res.Raw[OutputCapReasoningKey] != 32000 {
		t.Errorf("a final length/zero-output message must carry the stall marker: %v", res.Raw)
	}

	quiet := exportDoc(t, 0, 0, assistantMsg(1100, "unknown", 0, 0, 0, 0))
	res = Result{}
	applyOpencodeExport(&res, quiet, time.UnixMilli(1000), false)
	if !(opencode{}).ZeroUsageUnknown(res) {
		t.Errorf("a final unknown/zero-usage message must carry the quiet-death marker: %v", res.Raw)
	}

	// A clean final message clears a marker the stream parse left.
	clean := exportDoc(t, 100, 0, assistantMsg(1100, "stop", 100, 5, 0, 0))
	res = Result{Raw: map[string]any{TerminalReasonKey: ZeroUsageReason}}
	applyOpencodeExport(&res, clean, time.UnixMilli(1000), false)
	if _, ok := res.Raw[TerminalReasonKey]; ok {
		t.Errorf("terminal_reason survived a clean final message: %v", res.Raw)
	}

	// Our own wall-clock marker wins.
	res = Result{Raw: map[string]any{TerminalReasonKey: wallClockReason}}
	applyOpencodeExport(&res, quiet, time.UnixMilli(1000), false)
	if !(opencode{}).WallClockCapped(res) {
		t.Errorf("the wall-clock marker was overwritten: %v", res.Raw)
	}
}

// A run the provider never answered (a 401, an unknown model) has an export of
// zeroes; that is not a usage report, or IsUnclassifiedTransient would retry a
// startup refusal.
func TestApplyOpencodeExportDoesNotReportAnUnansweredRun(t *testing.T) {
	unanswered := opencodeExportMessage{Type: "assistant", Tokens: &opencodeExportTokens{}}
	unanswered.Time.Created = 1100
	doc := exportDoc(t, 0, 0, unanswered)
	res := Result{ExitCode: 1}
	applyOpencodeExport(&res, doc, time.UnixMilli(1000), false)
	if res.UsageReported {
		t.Error("UsageReported = true for a run with no answered message")
	}
	if (opencode{}).IsUnclassifiedTransient(res) {
		t.Error("an unanswered exit 1 leans retryable")
	}
}

func TestParseOpencodeExportRefusesADocumentWithoutTotals(t *testing.T) {
	if _, err := parseOpencodeExport([]byte(`{"info":{"id":"ses_x"},"messages":[]}`)); err == nil {
		t.Error("an export with no info.cost/info.tokens was accepted")
	}
	if _, err := parseOpencodeExport([]byte(`Session not found: ses_x`)); err == nil {
		t.Error("a non-JSON export was accepted")
	}
}

func TestIsResumeArgs(t *testing.T) {
	if !isResumeArgs(opencodeResumeArgs(Invocation{}, "ses_1", "go")) {
		t.Error("resume args not recognised")
	}
	if isResumeArgs(opencodeArgs(Invocation{Prompt: "-s"})) {
		t.Error("a prompt of -s faked a resume")
	}
}

// Through the real Invoke: the stub serves a session export after the run, and
// the Result carries its figures rather than the stream's.
func TestOpencodeInvokeReadsTheSessionExport(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "opencode")
	script := `#!/bin/sh
if [ "$1" = session ] && [ "$2" = export ]; then
  [ "$4" = ses_x ] || exit 1
  echo '{"info":{"id":"ses_x","cost":0.25,"tokens":{"input":900,"output":100,"reasoning":0,"cache":{"read":0,"write":0}}},"messages":[{"type":"assistant","finish":"stop","cost":0.2,"tokens":{"input":800,"output":100,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":99999999999999}}]}'
  exit 0
fi
echo '{"type":"step_start","sessionID":"ses_x","part":{"type":"step-start"}}'
echo '{"type":"text","sessionID":"ses_x","part":{"type":"text","text":"DONE"}}'
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "work"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Tokens != 1000 || res.CostUSD != 0.25 || !res.UsageReported {
		t.Errorf("Tokens/Cost/Reported = %d/%v/%v, want the export's 1000/0.25/true", res.Tokens, res.CostUSD, res.UsageReported)
	}
	if res.Raw[FinishReasonKey] != "stop" {
		t.Errorf("finish_reason = %v, want the export's stop", res.Raw[FinishReasonKey])
	}
}

// When the export cannot be read, the stream's step_finish sums stand.
func TestOpencodeInvokeFallsBackToTheStreamWhenTheExportFails(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '{"type":"step_finish","sessionID":"ses_x","part":{"type":"step-finish","reason":"tool-calls","tokens":{"input":10,"output":2,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.01}}'
echo '{"type":"step_start","sessionID":"ses_x","part":{"type":"step-start"}}'
echo '{"type":"text","sessionID":"ses_x","part":{"type":"text","text":"DONE"}}'
`)
	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "work"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.UsageReported || res.CostUSD != 0.01 {
		t.Errorf("stream fallback lost: reported=%v cost=%v", res.UsageReported, res.CostUSD)
	}
	if _, ok := res.Raw[FinishReasonKey]; ok {
		t.Errorf("finish_reason = %v; the final step never finished on the stream, so the last TOOL step's reason must not stand in for it", res.Raw[FinishReasonKey])
	}
}

// The session config: every server moved out of Code Mode into the 2.x
// dialect, 1.x `enabled` translated, the policy under `permission`.
func TestOpencodeSessionConfig(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "opencode-mcp.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	decode := func(t *testing.T, b []byte) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	servers := func(m map[string]any) map[string]any {
		return m["mcp"].(map[string]any)["servers"].(map[string]any)
	}

	t.Run("v1 dialect", func(t *testing.T) {
		b, err := opencodeSessionConfig(Invocation{MCPConfigPath: write(t,
			`{"model":"x/y","mcp":{"clankerbar":{"type":"remote","url":"https://clankerbar.com/mcp/a","enabled":true},"other":{"type":"local","command":["x"],"enabled":false}}}`)})
		if err != nil {
			t.Fatal(err)
		}
		m := decode(t, b)
		s := servers(m)
		cb := s["clankerbar"].(map[string]any)
		if cb["codemode"] != false || cb["url"] != "https://clankerbar.com/mcp/a" {
			t.Errorf("clankerbar = %v", cb)
		}
		if _, ok := cb["enabled"]; ok {
			t.Errorf("enabled survived: %v", cb)
		}
		other := s["other"].(map[string]any)
		if other["disabled"] != true || other["codemode"] != false {
			t.Errorf("a disabled 1.x server must come out disabled and out of Code Mode: %v", other)
		}
		if _, ok := m["mcp"].(map[string]any)["clankerbar"]; ok {
			t.Error("the 1.x entry was left beside its 2.x copy")
		}
		if m["model"] != "x/y" {
			t.Errorf("other keys must pass through: model = %v", m["model"])
		}
	})

	t.Run("v2 dialect", func(t *testing.T) {
		b, err := opencodeSessionConfig(Invocation{MCPConfigPath: write(t,
			`{"mcp":{"servers":{"clankerbar":{"type":"remote","url":"u","codemode":true,"timeout":30000}}}}`)})
		if err != nil {
			t.Fatal(err)
		}
		cb := servers(decode(t, b))["clankerbar"].(map[string]any)
		if cb["codemode"] != false {
			t.Errorf("an explicit codemode:true must be overridden: %v", cb)
		}
		if !strings.Contains(string(b), `"timeout":30000`) {
			t.Errorf("numbers must pass through as written: %s", b)
		}
	})

	t.Run("both dialects under one name", func(t *testing.T) {
		_, err := opencodeSessionConfig(Invocation{MCPConfigPath: write(t,
			`{"mcp":{"clankerbar":{"type":"remote","url":"a"},"servers":{"clankerbar":{"type":"remote","url":"b"}}}}`)})
		if err == nil {
			t.Error("a server declared in both dialects was accepted")
		}
	})

	t.Run("not JSON", func(t *testing.T) {
		if _, err := opencodeSessionConfig(Invocation{MCPConfigPath: write(t, `{"mcp": // comment
		}`)}); err == nil {
			t.Error("a non-JSON mcp config was accepted")
		}
	})

	t.Run("file permission replaced", func(t *testing.T) {
		b, err := opencodeSessionConfig(Invocation{MCPConfigPath: write(t, `{"permission":{"*":"allow"}}`)})
		if err != nil {
			t.Fatal(err)
		}
		if p := decode(t, b)["permission"].(map[string]any); p["*"] != "deny" {
			t.Errorf("permission = %v, want the adapter's", p)
		}
	})

	t.Run("no mcp config", func(t *testing.T) {
		// The policy must reach the session even with no file: 2.x without a
		// permission block runs every tool.
		b, err := opencodeSessionConfig(Invocation{WorkDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		m := decode(t, b)
		if len(m) != 1 || m["permission"].(map[string]any)["*"] != "deny" {
			t.Errorf("content = %s, want only the fail-closed permission block", b)
		}
	})
}

// 2.x exports no OPENCODE_PERMISSION: the variable is ignored by the binary,
// and exporting it would claim a posture that does not exist.
func TestOpencodeEnvCarriesThePolicyInTheContentLayer(t *testing.T) {
	env, err := opencode{}.env(Invocation{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ambient := os.LookupEnv("OPENCODE_PERMISSION"); !ambient {
		if got := lastValue(env, "OPENCODE_PERMISSION"); got != "" {
			t.Errorf("the adapter exported OPENCODE_PERMISSION=%s", got)
		}
	}
	if !strings.Contains(lastValue(env, "OPENCODE_CONFIG_CONTENT"), `"permission"`) {
		t.Error("OPENCODE_CONFIG_CONTENT carries no permission block")
	}
}

// Typed 2.x errors are recorded and classified: auth and no-route stop, a rate
// limit rides the backoff.
func TestOpencodeTypedErrors(t *testing.T) {
	for _, c := range []struct {
		line      string
		typ       string
		transient bool
	}{
		{`{"type":"error","sessionID":"ses_x","error":{"type":"provider.auth","message":"Incorrect API key provided","status":401}}`, "provider.auth", false},
		{`{"type":"error","sessionID":"ses_x","error":{"type":"provider.no-route","message":"Model unavailable: fake/x"}}`, "provider.no-route", false},
		{`{"type":"error","sessionID":"ses_x","error":{"type":"provider.rate-limit","message":"Rate limit reached for requests","status":429}}`, "provider.rate-limit", true},
	} {
		res := opencodeParsed(c.line)
		res.ExitCode = 1
		res.Stdout = c.line
		if res.Raw[OpencodeErrorTypeKey] != c.typ {
			t.Errorf("error type = %v, want %s", res.Raw[OpencodeErrorTypeKey], c.typ)
		}
		if res.Raw[opencodeSessionIDKey] != "ses_x" {
			t.Errorf("%s: an error-only stream must still yield its session id", c.typ)
		}
		if got := (opencode{}).IsTransient(res); got != c.transient {
			t.Errorf("%s: IsTransient = %v, want %v", c.typ, got, c.transient)
		}
	}
}

// The provider's call id repeats across steps (the fake numbers every step's
// calls from call_0), so replays are recognised by partID: a second, genuine
// call that reuses a call id must still be observed.
func TestOpencodeRepeatedCallIDsAcrossStepsAreBothObserved(t *testing.T) {
	failed := `{"type":"tool_use","sessionID":"ses_x","part":{"partID":"prt_1","type":"tool","id":"call_0","tool":"clankerbar_claim_task","state":{"status":"error","input":{"taskId":"T-9"},"error":"No tool named \"clankerbar_claim_task\" is currently available."}}}`
	ok := `{"type":"tool_use","sessionID":"ses_x","part":{"partID":"prt_2","type":"tool","id":"call_0","tool":"clankerbar_claim_task","state":{"status":"completed","input":{"taskId":"T-9"},"output":"{\"task\":{\"id\":\"T-9\",\"ref\":\"CLA-1\"},\"run\":{\"id\":\"run-1\"}}"}}}`
	res := opencodeParsed(failed + "\n" + ok)
	if !res.Claim.Held() || res.Claim.TaskID != "T-9" || res.Claim.RunID != "run-1" {
		t.Errorf("Claim = %+v; the retried claim after the MCP warm-up error was dropped as a replay", res.Claim)
	}
	// A true replay (same partID) is still ignored.
	res = opencodeParsed(ok + "\n" + ok)
	if !res.Claim.Held() {
		t.Errorf("Claim = %+v", res.Claim)
	}
}
