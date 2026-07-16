package harness

import (
	"bytes"
	"context"
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
	// TODO: parse the --json JSONL event stream — token_count /
	// turn.completed.usage for the Budget, turn.failed/error for limits.
	return res, nil
}

func (codex) DetectLimit(res Result) Limit {
	// TODO: match the provider 429 / "usage limit" in the JSONL error events.
	// Best-effort text scan until the parser lands.
	blob := res.Stdout + res.Stderr
	if strings.Contains(blob, "usage limit") || strings.Contains(blob, `"statusCode":429`) || strings.Contains(blob, "rate limit") {
		return Limit{Limited: true, Reason: "usage_limit"}
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
