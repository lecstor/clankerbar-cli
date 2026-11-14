package loop

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// The 402 body from the incident: the provider's own message is nested under
// error.message, with the HTTP status beside it.
const limitStop402 = `{"type":"error","error":{"type":"provider.quota","message":"Upstream request failed: Insufficient account funds","status":402}}`

// realClassifyFake scripts Invoke (like fakeAdapter) but classifies with the
// REAL opencode adapter, so these tests exercise the production limit scan and
// diagnostic scope against the fixtures instead of a scripted verdict.
type realClassifyFake struct {
	*fakeAdapter
}

func (f realClassifyFake) DetectLimit(res harness.Result) harness.Limit {
	return opencodeAdapter().DetectLimit(res)
}

func (f realClassifyFake) Diagnostic(res harness.Result) string {
	return opencodeAdapter().Diagnostic(res)
}

func opencodeAdapter() harness.Adapter {
	a, err := harness.Get("opencode")
	if err != nil {
		panic(err)
	}
	return a
}

// A Stop-class limit records the reason, the model route that stopped and the
// provider's own message in the marker's first line — the three things the
// original incident needed an ssh session to recover.
func TestLimitStopLineNamesTheReasonRouteAndMessage(t *testing.T) {
	a := opencodeAdapter()
	res := harness.Result{Stdout: limitStop402, Raw: map[string]any{
		harness.RouteStartedKey:  "opencode-free/deepseek-v4.1-flash",
		harness.RouteFinishedKey: "opencode-go/deepseek-v4.1-flash",
	}}
	lim := a.DetectLimit(res)
	if !lim.Stop {
		t.Fatalf("DetectLimit(%s) = %+v, want a Stop", limitStop402, lim)
	}

	got := limitStopLine(lim, limitStopRoute(res, "opencode-free/deepseek-v4.1-flash"), limitStopMessage(a, res))
	want := "budget_exhausted on opencode-go/deepseek-v4.1-flash: Upstream request failed: Insufficient account funds"
	if got != want {
		t.Errorf("marker line = %q, want %q", got, want)
	}
}

// The route named is the one that ended the session: after a switch it is the
// LAST route, never the one the session started on. A session that never
// switched is on its configured model.
func TestLimitStopRouteNamesTheRouteThatStopped(t *testing.T) {
	switched := harness.Result{Raw: map[string]any{
		harness.RouteStartedKey:  "free/deepseek",
		harness.RouteFinishedKey: "paid/deepseek",
	}}
	if got := limitStopRoute(switched, "free/deepseek"); got != "paid/deepseek" {
		t.Errorf("switched session route = %q, want the last route paid/deepseek", got)
	}
	if got := limitStopRoute(harness.Result{}, "configured/model"); got != "configured/model" {
		t.Errorf("unswitched session route = %q, want the configured model", got)
	}
	if got := limitStopRoute(harness.Result{}, ""); got != "" {
		t.Errorf("unknown route = %q, want empty so the marker omits it", got)
	}
}

// A marker line omits the parts it does not know, and a reason-less limit keeps
// the original 'usage limit' fallback.
func TestLimitStopLineOmitsWhatItDoesNotKnow(t *testing.T) {
	tests := []struct {
		name  string
		lim   harness.Limit
		route string
		msg   string
		want  string
	}{
		{"everything known", harness.Limit{Reason: "budget_exhausted"}, "r/one", "no funds", "budget_exhausted on r/one: no funds"},
		{"no message", harness.Limit{Reason: "budget_exhausted"}, "r/one", "", "budget_exhausted on r/one"},
		{"no route", harness.Limit{Reason: "budget_exhausted"}, "", "no funds", "budget_exhausted: no funds"},
		{"neither", harness.Limit{Reason: "budget_exhausted"}, "", "", "budget_exhausted"},
		{"blank reason keeps the fallback", harness.Limit{}, "r/one", "no funds", "usage limit on r/one: no funds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := limitStopLine(tc.lim, tc.route, tc.msg); got != tc.want {
				t.Errorf("limitStopLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A provider message longer than the marker's read is clipped, and the line is
// one printable line whatever the diagnostic carried. The long fixture mixes
// multi-byte runes into the clip boundary on purpose: a byte-slice clip there
// would split a rune and fail the UTF-8 assertion below.
func TestLimitStopLineIsClippedToOneLine(t *testing.T) {
	long := "upstream failure " + strings.Repeat("·→", 40) + "\nsecond line"
	// Guard the fixture itself: the clip boundary must fall inside a multi-byte
	// rune, or this test could not catch a byte-slice clip.
	raw := oneLine("budget_exhausted on route/one second route: " + long)
	if utf8.ValidString(raw[:limitStopLineMax-3]) {
		t.Fatalf("fixture does not exercise a rune-boundary clip: raw[:%d] is valid UTF-8", limitStopLineMax-3)
	}
	line := limitStopLine(harness.Limit{Stop: true, Reason: "budget_exhausted"}, "route/one\r\nsecond route", long)

	if len(line) > limitStopLineMax {
		t.Errorf("line is %d bytes, want <= %d so the supervisor's 200-byte read sees it whole", len(line), limitStopLineMax)
	}
	if strings.ContainsAny(line, "\n\r\x1b") {
		t.Errorf("line = %q, want no newline or control bytes", line)
	}
	if !utf8.ValidString(line) {
		t.Errorf("line = %q, want valid UTF-8", line)
	}
	if !strings.HasSuffix(line, "...") {
		t.Errorf("line = %q, want the clip marked with ...", line)
	}
	if !strings.HasPrefix(line, "budget_exhausted on route/one second route: upstream failure") {
		t.Errorf("line = %q, want the reason and route kept ahead of the clipped message", line)
	}
}

// The marker never quotes the agent's narration: only the adapter's error scope
// (stderr plus the terminal error event) is read, the same source DetectLimit
// uses, so a task body or assistant text mentioning the failure cannot reach
// the console.
func TestLimitStopMessageNeverQuotesAssistantText(t *testing.T) {
	a := opencodeAdapter()
	res := harness.Result{Stdout: strings.Join([]string{
		`{"type":"text","sessionID":"ses_1","part":{"type":"text","text":"The task body says: insufficient funds, so I stopped."}}`,
		limitStop402,
	}, "\n")}

	if !a.DetectLimit(res).Stop {
		t.Fatal("the 402 terminal error event did not classify as a Stop")
	}
	got := limitStopMessage(a, res)
	if strings.Contains(got, "task body") || strings.Contains(got, "so I stopped") {
		t.Errorf("marker message = %q, must not carry assistant narration", got)
	}
	if got != "Upstream request failed: Insufficient account funds" {
		t.Errorf("marker message = %q, want the provider's own message", got)
	}

	// A stream whose only mention of the phrase is the assistant's does not
	// classify as a limit at all, so no marker is written for it.
	narrationOnly := harness.Result{Stdout: `{"type":"text","sessionID":"ses_1","part":{"type":"text","text":"insufficient funds"}}`}
	if lim := a.DetectLimit(narrationOnly); lim.Limited {
		t.Errorf("assistant narration classified as a limit: %+v", lim)
	}
}

// The provider's message is read from the shapes adapters actually emit: the
// opencode wrapper, its older APIError data envelope, and an event that carries
// the error as a plain string.
func TestLimitStopMessageReadsTheProvidersOwnField(t *testing.T) {
	tests := []struct {
		name string
		diag string
		want string
	}{
		{"opencode 2.x error.message", limitStop402, "Upstream request failed: Insufficient account funds"},
		{"opencode APIError data.message", `{"type":"error","error":{"data":{"message":"Payment Required","statusCode":402}}}`, "Payment Required"},
		{"error as a bare string", `{"type":"error","error":"rate limit reached"}`, "rate limit reached"},
		{"no typed error event", "Error: Out of credits", ""},
		{"nothing said", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalErrorMessage(tc.diag); got != tc.want {
				t.Errorf("terminalErrorMessage(%q) = %q, want %q", tc.diag, got, tc.want)
			}
		})
	}
}

// With no typed error event to read, the adapter's diagnostic text itself is
// the message (stderr is the harness's own channel for every adapter).
func TestLimitStopMessageFallsBackToTheDiagnostic(t *testing.T) {
	f := &fakeAdapter{}
	res := harness.Result{Stderr: "Error: Out of credits\x1b[0m\ncontact billing"}
	if got := limitStopMessage(f, res); got != "Error: Out of credits[0m contact billing" {
		t.Errorf("limitStopMessage = %q, want the scoped diagnostic on one line", got)
	}
}

// The marker on disk carries the composed line, and the drain reads it back the
// way the supervisor does.
func TestDrainWithRetries_StopLimitMarkerNamesTheRouteAndMessage(t *testing.T) {
	res := harness.Result{ExitCode: 1, Stdout: limitStop402, Raw: map[string]any{
		harness.RouteStartedKey:  "opencode-free/deepseek-v4.1-flash",
		harness.RouteFinishedKey: "opencode-go/deepseek-v4.1-flash",
	}}
	d := New(fastCfg(), realClassifyFake{&fakeAdapter{steps: []invokeStep{{res: res}}}}, &fakePoller{})
	openTestStateDir(t, d)
	if _, _, stop, _ := d.drainWithRetries(context.Background(), 1, 0, d.targets[0], spend{start: time.Now()}); !stop {
		t.Fatal("stop = false, want the Stop-class limit to end the run")
	}
	present, reason := d.readMarker(LimitStopName)
	want := "budget_exhausted on opencode-go/deepseek-v4.1-flash: Upstream request failed: Insufficient account funds"
	if !present || reason != want {
		t.Errorf("marker = (%v, %q), want present with %q", present, reason, want)
	}
}

// A single-route session names the model its invocation was configured with.
func TestDrainWithRetries_StopLimitMarkerNamesTheConfiguredModel(t *testing.T) {
	cfg := fastCfg()
	cfg.Model = "opencode-go/deepseek-v4.1-flash"
	res := harness.Result{ExitCode: 1, Stdout: limitStop402}
	d := New(cfg, realClassifyFake{&fakeAdapter{steps: []invokeStep{{res: res}}}}, &fakePoller{})
	openTestStateDir(t, d)
	if _, _, stop, _ := d.drainWithRetries(context.Background(), 1, 0, d.targets[0], spend{start: time.Now()}); !stop {
		t.Fatal("stop = false, want the Stop-class limit to end the run")
	}
	present, reason := d.readMarker(LimitStopName)
	want := "budget_exhausted on opencode-go/deepseek-v4.1-flash: Upstream request failed: Insufficient account funds"
	if !present || reason != want {
		t.Errorf("marker = (%v, %q), want present with %q", present, reason, want)
	}
}
