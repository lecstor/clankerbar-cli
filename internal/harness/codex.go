package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
)

func init() { Register(codex{}) }

// codex drives OpenAI's Codex CLI (`codex exec`). Divergences from Claude Code,
// per the spike, that this adapter has to absorb:
//   - Permissions are two axes: --sandbox {read-only|workspace-write|...} and
//     --ask-for-approval {untrusted|on-request|never}. "edits auto, shell gated"
//     ≈ `-s workspace-write -a never`.
//   - Output convention inverts: final message on stdout, events on stderr,
//     unless --json (then a JSONL event stream on stdout).
//   - No stable limit exit code and rate_limits is null in exec --json, so limit
//     detection is fuzzy text-matching and there is nothing to introspect.
type codex struct{}

func (codex) Name() string { return "codex" }

func (c codex) Invoke(ctx context.Context, in Invocation) (Result, error) {
	var args []string
	if in.Probe {
		args = []string{"exec", ".", "--json", "--sandbox", "read-only", "--ask-for-approval", "never"}
	} else {
		args = []string{"exec", in.Prompt, "--json", "--sandbox", "workspace-write", "--ask-for-approval", "never"}
		if in.Model != "" {
			args = append(args, "-m", in.Model)
		}
	}

	cmd := exec.CommandContext(ctx, "codex", args...)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = append(os.Environ(), in.Env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if runErr != nil {
		return res, runErr
	}
	c.parse(&res)
	return res, nil
}

// codexEvent is one line of the `codex exec --json` JSONL stream. The schema is
// experimental/underdocumented, so this captures only the fields we need and
// tolerates everything else. Field names follow the documented shape (the
// capability spike); unknown/missing fields are simply skipped.
type codexEvent struct {
	Type            string      `json:"type"`
	Usage           *codexUsage `json:"usage"` // on turn.completed
	InputTokens     *int        `json:"input_tokens"`
	CachedInput     *int        `json:"cached_input_tokens"`
	OutputTokens    *int        `json:"output_tokens"`
	ReasoningTokens *int        `json:"reasoning_output_tokens"`
	Text            string      `json:"text"`
	Item            *codexItem  `json:"item"`
}

type codexUsage struct {
	InputTokens     int `json:"input_tokens"`
	CachedInput     int `json:"cached_input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ReasoningTokens int `json:"reasoning_output_tokens"`
}

type codexItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// text returns the assistant message text this event carries, if any — captured
// best-effort, preferring items that look like agent messages over tool output.
func (ev codexEvent) text() string {
	if ev.Item != nil && ev.Item.Text != "" {
		if ev.Item.Type == "" || strings.Contains(ev.Item.Type, "message") || strings.Contains(ev.Item.Type, "agent") {
			return ev.Item.Text
		}
	}
	if ev.Text != "" && strings.Contains(ev.Type, "message") {
		return ev.Text
	}
	return ""
}

// parse walks the JSONL stream to fill FinalMessage and Tokens (for the Budget).
func (codex) parse(res *Result) {
	var (
		lastText                string
		in, cached, out, reason int
		sawUsage                bool
	)
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev codexEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue // partial/non-JSON line — skip
		}
		// Take the LAST usage-bearing event as the session total. This assumes
		// turn.completed / token_count report cumulative usage.
		// TODO: confirm cumulative-vs-delta against real `codex exec --json`.
		switch {
		case ev.Usage != nil:
			in, cached, out, reason = ev.Usage.InputTokens, ev.Usage.CachedInput, ev.Usage.OutputTokens, ev.Usage.ReasoningTokens
			sawUsage = true
		case ev.InputTokens != nil || ev.OutputTokens != nil:
			in, cached, out, reason = deref(ev.InputTokens), deref(ev.CachedInput), deref(ev.OutputTokens), deref(ev.ReasoningTokens)
			sawUsage = true
		}
		if t := ev.text(); t != "" {
			lastText = t
		}
	}
	res.FinalMessage = lastText
	if sawUsage {
		// Exclude cached input (discounted reads); reasoning counts as output spend.
		res.Tokens = in + out + reason
		res.Raw = map[string]any{
			"input_tokens": in, "cached_input_tokens": cached,
			"output_tokens": out, "reasoning_output_tokens": reason,
		}
	}
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func (codex) DetectLimit(res Result) Limit {
	// Codex has no stable limit exit code and (exec --json) emits rate_limits:null,
	// so limit detection is a best-effort text scan of the JSONL error/turn.failed
	// events and stderr. Reset time is not exposed structurally, so ResetAt stays
	// zero and the loop leans on interval probing.
	blob := strings.ToLower(res.Stdout + res.Stderr)
	for _, needle := range []string{"usage limit", "rate limit", "too many requests", `"statuscode":429`, `"status":429`} {
		if strings.Contains(blob, needle) {
			return Limit{Limited: true, Reason: "usage_limit"}
		}
	}
	return Limit{}
}

func (c codex) Probe(ctx context.Context, in Invocation) (Limit, error) {
	in.Probe = true
	res, err := c.Invoke(ctx, in)
	if err != nil {
		return Limit{}, err
	}
	return c.DetectLimit(res), nil
}

func (codex) ReadUsage(context.Context, Invocation) (Usage, error) {
	// exec --json emits rate_limits: null; /status is TUI-only (openai/codex#14728).
	return Usage{}, ErrUsageUnsupported
}
