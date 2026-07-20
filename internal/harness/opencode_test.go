package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

// A representative single-turn `opencode run --format json` stream, captured from
// opencode 1.18.2. One step: a step-start, the final text part, and a step-finish
// carrying that step's tokens/cost. The parser is defensive, so a non-JSON line is
// interleaved to prove it is skipped.
const opencodeStream = `{"type":"step_start","sessionID":"ses_1","part":{"type":"step-start","messageID":"msg_1"}}
not json, should be skipped
{"type":"text","sessionID":"ses_1","part":{"type":"text","text":"Backlog drained.","time":{"start":1,"end":2}}}
{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"stop","tokens":{"total":18909,"input":3,"output":4,"reasoning":0,"cache":{"write":18902,"read":0}},"cost":0.0236505}}`

func TestOpencodeParse(t *testing.T) {
	res := Result{Stdout: opencodeStream}
	opencode{}.parse(&res)

	if res.FinalMessage != "Backlog drained." {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "Backlog drained.")
	}
	// The single step-finish reports total=18909 (it already folds in cache).
	if want := 18909; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d", res.Tokens, want)
	}
	if res.CostUSD != 0.0236505 {
		t.Errorf("CostUSD = %v, want %v", res.CostUSD, 0.0236505)
	}
}

// A multi-turn drain emits several step-finish parts (one per turn) and an
// intermediate text part before the final one. Tokens SUM across turns; the final
// message is the LAST text part.
func TestOpencodeParse_multiStep(t *testing.T) {
	const stream = `{"type":"text","part":{"type":"text","text":"Working on it..."}}
{"type":"step_finish","part":{"type":"step-finish","tokens":{"total":100,"input":40,"output":10,"reasoning":0,"cache":{"write":50,"read":0}},"cost":0.01}}
{"type":"text","part":{"type":"text","text":"Done: opened PR #7."}}
{"type":"step_finish","part":{"type":"step-finish","tokens":{"total":200,"input":80,"output":20,"reasoning":0,"cache":{"write":100,"read":0}},"cost":0.02}}`
	res := Result{Stdout: stream}
	opencode{}.parse(&res)

	if res.FinalMessage != "Done: opened PR #7." {
		t.Errorf("FinalMessage = %q, want the last text part", res.FinalMessage)
	}
	if want := 100 + 200; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d (summed across steps)", res.Tokens, want)
	}
	if res.CostUSD != 0.03 {
		t.Errorf("CostUSD = %v, want 0.03 (summed across steps)", res.CostUSD)
	}
}

func TestOpencodeParse_empty(t *testing.T) {
	res := Result{Stdout: ""}
	opencode{}.parse(&res)
	if res.FinalMessage != "" || res.Tokens != 0 {
		t.Errorf("empty stream: got msg=%q tokens=%d, want empty/0", res.FinalMessage, res.Tokens)
	}
}

func TestOpencodeDetectLimit(t *testing.T) {
	// DetectLimit is the HARD budget/credit-exhaustion STOP only; an API 429 rate
	// limit is transient (below), not this.
	cases := []struct {
		name string
		blob string
		want bool
	}{
		{"402 payment required (opencode error event)", `{"type":"error","error":{"data":{"message":"Payment Required","statusCode":402,"isRetryable":false}}}`, true},
		{"out of credits prose", "Your account is out of credits", true},
		{"insufficient credits", `{"error":{"message":"Insufficient credits to run this request"}}`, true},
		{"openrouter credit balance too low", "Your credit balance is too low to access this model", true},
		{"monthly limit reached", "You have reached your monthly limit", true},
		{"spend cap", "spend cap exceeded for this workspace", true},
		{"429 is transient not the cap", `{"type":"error","error":{"data":{"statusCode":429,"isRetryable":true}}}`, false},
		{"clean run", opencodeStream, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (opencode{}).DetectLimit(Result{Stdout: tc.blob})
			if got.Limited != tc.want {
				t.Errorf("DetectLimit.Limited = %v, want %v", got.Limited, tc.want)
			}
			// A recognized budget stop must flag Stop so the loop stops rather than
			// waiting for a reset that never comes.
			if tc.want && !got.Stop {
				t.Errorf("DetectLimit recognized a budget limit but did not set Stop")
			}
		})
	}
}

func TestOpencodeIsTransient(t *testing.T) {
	cases := []struct {
		name string
		blob string
		want bool
	}{
		{"429 statusCode", `{"type":"error","error":{"data":{"statusCode":429}}}`, true},
		{"5xx statusCode", `{"error":{"data":{"statusCode":503}}}`, true},
		{"isRetryable flag", `{"type":"error","error":{"data":{"statusCode":529,"isRetryable":true}}}`, true},
		{"too many requests", "Error: 429 Too Many Requests", true},
		{"connection blip", "fetch failed", true},
		{"overloaded", "provider overloaded", true},
		{"402 payment required is NOT transient", `{"error":{"data":{"statusCode":402,"isRetryable":false}}}`, false},
		{"budget exhaustion is NOT transient", "Your account is out of credits", false},
		{"clean run", opencodeStream, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (opencode{}).IsTransient(Result{Stdout: tc.blob}); got != tc.want {
				t.Errorf("IsTransient = %v, want %v", got, tc.want)
			}
		})
	}
}

// Probe must build a READ-ONLY invocation: a trivial prompt and a permission
// policy that denies edits, shell and the exfil tools — zero writes.
func TestOpencodeProbeIsReadOnly(t *testing.T) {
	args := opencodeArgs(Invocation{Probe: true, Prompt: "Work the backlog.", Model: "opencode/claude-haiku-4-5"})
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "run . --format json") {
		t.Errorf("probe args = %q, want a trivial `run . --format json` request", joined)
	}
	for _, a := range args {
		if a == "Work the backlog." {
			t.Error("probe leaked the real drain prompt into its invocation")
		}
	}

	perm := opencodePermission(true) // readOnly
	var p map[string]string
	if err := json.Unmarshal([]byte(perm), &p); err != nil {
		t.Fatalf("permission policy is not valid JSON: %v", err)
	}
	for _, tool := range []string{"edit", "bash", "webfetch", "websearch", "external_directory"} {
		if p[tool] != "deny" {
			t.Errorf("read-only policy: %s = %q, want deny", tool, p[tool])
		}
	}
}

// A real drain fails closed on exfil but must still allow edits and shell to do
// the work; the prompt is carried through verbatim.
func TestOpencodeDrainInvocation(t *testing.T) {
	args := opencodeArgs(Invocation{Prompt: "Work the backlog.", Model: "opencode/claude-sonnet-4-5"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "run Work the backlog. --format json") {
		t.Errorf("drain args = %q, want the prompt carried into `opencode run`", joined)
	}
	if !strings.Contains(joined, "--model opencode/claude-sonnet-4-5") {
		t.Errorf("drain args = %q, want the model passed through", joined)
	}

	perm := opencodePermission(false) // drain
	var p map[string]string
	if err := json.Unmarshal([]byte(perm), &p); err != nil {
		t.Fatalf("permission policy is not valid JSON: %v", err)
	}
	if p["edit"] != "allow" || p["bash"] != "allow" {
		t.Errorf("drain policy must allow edit+bash, got edit=%q bash=%q", p["edit"], p["bash"])
	}
	for _, tool := range []string{"webfetch", "websearch", "external_directory"} {
		if p[tool] != "deny" {
			t.Errorf("drain policy must deny exfil tool %s, got %q", tool, p[tool])
		}
	}
}

func TestOpencodeReadUsageUnsupported(t *testing.T) {
	if _, err := (opencode{}).ReadUsage(nil, Invocation{}); err != ErrUsageUnsupported {
		t.Errorf("ReadUsage err = %v, want ErrUsageUnsupported", err)
	}
}

func TestOpencodeRegistered(t *testing.T) {
	a, err := Get("opencode")
	if err != nil {
		t.Fatalf("opencode not registered: %v", err)
	}
	if a.Name() != "opencode" {
		t.Errorf("Get(opencode).Name() = %q", a.Name())
	}
}
