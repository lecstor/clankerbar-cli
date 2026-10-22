package harness

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLA-782: model route fallback. A usage limit on one route resumes the SAME
// opencode session on the next, so the transcript survives; a transient blip is
// not a switch; and a limit on the last route is left for the driver unchanged.

const (
	routeFree = "opencode/deepseek-flash-free"
	routePaid = "opencode-go/deepseek-flash"
)

// routeStubScript writes its argument list to <dir>/log, then answers per call:
// the first (no `-s`) is denied by the fixture's error event, the continuation
// (with `-s`) is clean. The caller supplies the first run's error line so one
// script shape covers a waitable limit, a hard stop and a transient blip.
func routeStubScript(t *testing.T, firstError string) (script, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "log")
	script = `#!/bin/sh
echo "$*" >> "` + logPath + `"
case "$*" in
  *"-s ses_live"*)
    echo '{"type":"text","sessionID":"ses_live","part":{"type":"text","text":"continuing on the paid route"}}'
    echo '{"type":"step_finish","sessionID":"ses_live","part":{"type":"step-finish","reason":"stop","tokens":{"total":80,"input":60,"output":20,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.002}}'
    ;;
  *)
    echo '{"type":"text","sessionID":"ses_live","part":{"type":"text","text":"working on the free route"}}'
    echo '{"type":"step_finish","sessionID":"ses_live","part":{"type":"step-finish","reason":"stop","tokens":{"total":120,"input":100,"output":20,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.001}}'
    echo '` + firstError + `'
    exit 1
    ;;
esac
`
	return script, logPath
}

// stubCalls returns the argument list of every run the stub served, one per line.
func stubCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("stub log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

// A waitable quota on route 1 is followed by a resumed run on route 2: same
// session id, the next `--model`, a clean result, and both runs' spend summed.
func TestOpencodeInvokeSwitchesRouteOnAUsageLimit(t *testing.T) {
	script, logPath := routeStubScript(t,
		`{"type":"error","sessionID":"ses_live","error":{"type":"provider.quota","message":"Usage limit reached. Resets at 2026-10-23T00:00:00Z"}}`)
	opencodeStub(t, script)

	console := &bytes.Buffer{}
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:  "Work.",
		Model:   routeFree,
		Routes:  []string{routeFree, routePaid},
		Console: console,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	calls := stubCalls(t, logPath)
	if len(calls) != 2 {
		t.Fatalf("the stub ran %d times, want 2 (free route + one paid continuation):\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if strings.Contains(calls[0], "-s ") {
		t.Errorf("the first run was a resume: %s", calls[0])
	}
	if !strings.Contains(calls[0], "--model "+routeFree) {
		t.Errorf("the first run did not start on the free route: %s", calls[0])
	}
	// The switch: same session, next route, the short continuation prompt.
	if !strings.Contains(calls[1], "-s ses_live") {
		t.Errorf("the switch did not resume the same session: %s", calls[1])
	}
	if !strings.Contains(calls[1], "--model "+routePaid) {
		t.Errorf("the switch did not move to the paid route: %s", calls[1])
	}
	if !strings.Contains(calls[1], "previous model route hit a usage limit") {
		t.Errorf("the continuation did not tell the session what happened: %s", calls[1])
	}

	// The merged Result is the continuation's own: clean, not limited, and the
	// free route's stream did not leak its limit into the classification.
	if lim := (opencode{}).DetectLimit(res); lim.Limited {
		t.Errorf("the merged Result still reads as limited (%+v) — the abandoned route's diagnostics survived the merge", lim)
	}
	if (opencode{}).IsTransient(res) {
		t.Error("a clean continuation was classified transient")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want the continuation's 0", res.ExitCode)
	}
	// Spend sums: 120 on the free route + 80 on the paid one.
	if res.Tokens != 200 {
		t.Errorf("Tokens = %d, want 200 summed across both routes", res.Tokens)
	}
	if res.CostUSD != 0.003 {
		t.Errorf("CostUSD = %v, want 0.003 summed across both routes", res.CostUSD)
	}

	started, finished, fallbacks := RouteFallbacks(res)
	if started != routeFree || finished != routePaid {
		t.Errorf("route report = %q -> %q, want %q -> %q", started, finished, routeFree, routePaid)
	}
	if len(fallbacks) != 1 {
		t.Fatalf("fallbacks = %+v, want exactly the free route", fallbacks)
	}
	if fallbacks[0].Route != routeFree || fallbacks[0].Next != routePaid || fallbacks[0].Reason != "usage_limit" {
		t.Errorf("fallback = %+v, want free -> paid (usage_limit)", fallbacks[0])
	}
	if fallbacks[0].ResetAt.IsZero() {
		t.Error("the provider's stated reset was dropped from the fallback report")
	}
	if !strings.Contains(console.String(), "resuming the session on "+routePaid) {
		t.Errorf("console missing the switch line; got:\n%s", console.String())
	}
}

// A transient 429 is NOT a switch: it rides the existing retry ladder, on the
// route that already holds the context.
func TestOpencodeInvokeTransientRateLimitDoesNotSwitch(t *testing.T) {
	script, logPath := routeStubScript(t,
		`{"type":"error","sessionID":"ses_live","error":{"type":"provider.rate-limit","message":"429 Too Many Requests"}}`)
	opencodeStub(t, script)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt: "Work.",
		Model:  routeFree,
		Routes: []string{routeFree, routePaid},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if calls := stubCalls(t, logPath); len(calls) != 1 {
		t.Fatalf("the stub ran %d times, want 1 — a transient 429 must not switch routes:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if !(opencode{}).IsTransient(res) {
		t.Error("a provider.rate-limit was not classified transient")
	}
	if _, _, fallbacks := RouteFallbacks(res); len(fallbacks) != 0 {
		t.Errorf("a transient blip reported route fallbacks: %+v", fallbacks)
	}
}

// A hard Stop on a route that is NOT the last is just a switch: a credit
// exhaustion on the free route's account says nothing about the paid route's.
func TestOpencodeInvokeStopOnANonLastRouteSwitches(t *testing.T) {
	script, logPath := routeStubScript(t,
		`{"type":"error","sessionID":"ses_live","error":{"type":"unknown","message":"You are out of credits"}}`)
	opencodeStub(t, script)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt: "Work.",
		Model:  routeFree,
		Routes: []string{routeFree, routePaid},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if calls := stubCalls(t, logPath); len(calls) != 2 {
		t.Fatalf("the stub ran %d times, want 2 — a Stop on a non-last route must switch, not end the session:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if lim := (opencode{}).DetectLimit(res); lim.Limited {
		t.Errorf("the merged Result is still limited (%+v) after the paid route finished cleanly", lim)
	}
	_, finished, fallbacks := RouteFallbacks(res)
	if finished != routePaid || len(fallbacks) != 1 || fallbacks[0].Reason != "budget_exhausted" {
		t.Errorf("finished=%q fallbacks=%+v, want a paid finish after a budget_exhausted stop", finished, fallbacks)
	}
}

// All routes exhausted: the last route's limit is left on the Result unchanged,
// so the driver's existing branch waits it out or stops the run.
func TestOpencodeInvokeLastRouteLimitStands(t *testing.T) {
	const secondReset = "2026-10-24T09:30:00Z"
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
case "$*" in
  *"-s ses_live"*)
    echo '{"type":"error","sessionID":"ses_live","error":{"type":"provider.quota","message":"Usage limit reached. Resets at `+secondReset+`"}}'
    exit 1
    ;;
  *)
    echo '{"type":"error","sessionID":"ses_live","error":{"type":"provider.quota","message":"Usage limit reached. Resets at 2026-10-23T00:00:00Z"}}'
    exit 1
    ;;
esac
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt: "Work.",
		Model:  routeFree,
		Routes: []string{routeFree, routePaid},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if calls := stubCalls(t, logPath); len(calls) != 2 {
		t.Fatalf("the stub ran %d times, want 2 (one switch, then the last route's limit):\n%s", len(calls), strings.Join(calls, "\n"))
	}
	lim := (opencode{}).DetectLimit(res)
	if !lim.Limited {
		t.Fatal("the last route's limit was swallowed; the driver would treat a denied session as a clean one")
	}
	if lim.Stop {
		t.Errorf("the last route's waitable limit became a Stop (%+v)", lim)
	}
	if got := lim.ResetAt.UTC().Format("2006-01-02T15:04:05Z"); got != secondReset {
		t.Errorf("ResetAt = %s, want the LAST route's stated reset %s", got, secondReset)
	}
	_, finished, fallbacks := RouteFallbacks(res)
	if finished != routePaid {
		t.Errorf("finished = %q, want the paid route (the one that ended the session)", finished)
	}
	if len(fallbacks) != 2 || fallbacks[1].Next != "" {
		t.Errorf("fallbacks = %+v, want both routes with the last one having no Next", fallbacks)
	}
}

// The single-route pin: adding Routes must not move one byte of the argv a
// config that names one route (or a plain string) produced before.
func TestOpencodeSingleRouteArgvIsUnchanged(t *testing.T) {
	old := opencodeArgs(Invocation{Prompt: "Work.", Model: routeFree})
	same := opencodeArgs(Invocation{Prompt: "Work.", Model: routeFree, Routes: []string{routeFree}})
	want := []string{"run", "--standalone", "--format", "json", "--agent", "build", "--model", routeFree, "--", "Work."}
	if len(old) != len(want) {
		t.Fatalf("argv = %v, want %v", old, want)
	}
	for i := range want {
		if old[i] != want[i] {
			t.Fatalf("argv = %v, want %v", old, want)
		}
	}
	if len(same) != len(old) {
		t.Fatalf("a one-route list changed the argv: %v vs %v", same, old)
	}
	for i := range old {
		if same[i] != old[i] {
			t.Fatalf("a one-route list changed the argv: %v vs %v", same, old)
		}
	}
}

// A clean single-route session reports nothing, so its Raw keeps exactly the
// shape it had before the feature existed.
func TestOpencodeCleanSessionCarriesNoRouteReport(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
echo '{"type":"text","sessionID":"ses_clean","part":{"type":"text","text":"done"}}'
echo '{"type":"step_finish","sessionID":"ses_clean","part":{"type":"step-finish","reason":"stop","tokens":{"total":10,"input":8,"output":2,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.001}}'
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "Work.", Model: routeFree})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if started, finished, fallbacks := RouteFallbacks(res); started != "" || finished != "" || len(fallbacks) != 0 {
		t.Errorf("a single-route clean session reported routes: %q -> %q %+v", started, finished, fallbacks)
	}
}
