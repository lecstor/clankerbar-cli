package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Usage accounting on opencode 2.x.

const (
	// OpencodeErrorTypeKey is the Result.Raw key carrying the type of the last typed error event a 2.x
	// session emitted ("provider.auth", ...).
	OpencodeErrorTypeKey = "opencode_error_type"

	// opencodeUsageSourceKey records where a Result's usage figures came from: "session_export" when
	// the export was read, absent when the stream sums stand (the export failed or the run produced no
	// session).
	opencodeUsageSourceKey = "usage_source"
	opencodeUsageExport    = "session_export"
)

// opencodeExportTimeout bounds one export.
var opencodeExportTimeout = 15 * time.Second

type opencodeExportTokens struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

func (t opencodeExportTokens) total() int {
	return t.Input + t.Output + t.Reasoning + t.Cache.Read + t.Cache.Write
}

func (t *opencodeExportTokens) add(o opencodeExportTokens, sign int) {
	t.Input += sign * o.Input
	t.Output += sign * o.Output
	t.Reasoning += sign * o.Reasoning
	t.Cache.Read += sign * o.Cache.Read
	t.Cache.Write += sign * o.Cache.Write
}

type opencodeExportMessage struct {
	Type   string                `json:"type"`
	Finish string                `json:"finish"`
	Cost   float64               `json:"cost"`
	Tokens *opencodeExportTokens `json:"tokens"`
	Time   struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

// opencodeExport is the part of `opencode session export` the adapter reads.
type opencodeExport struct {
	Info struct {
		ID     string                `json:"id"`
		Cost   *float64              `json:"cost"`
		Tokens *opencodeExportTokens `json:"tokens"`
	} `json:"info"`
	Messages []opencodeExportMessage `json:"messages"`
}

// parseOpencodeExport decodes an export and refuses one without the session totals: a document
// missing them is not something to account against.
func parseOpencodeExport(data []byte) (opencodeExport, error) {
	var doc opencodeExport
	if err := json.Unmarshal(bytes.TrimSpace(data), &doc); err != nil {
		return doc, fmt.Errorf("session export is not JSON: %w", err)
	}
	if doc.Info.Cost == nil || doc.Info.Tokens == nil {
		return doc, errors.New("session export carries no info.cost/info.tokens")
	}
	return doc, nil
}

// settleUsage replaces a finished run's streamed usage with the session export's figures when the
// export can be read, and leaves the stream sums in place when it cannot.
func (o opencode) settleUsage(ctx context.Context, in Invocation, res *Result, started time.Time, resume bool) {
	sid, _ := res.Raw[opencodeSessionIDKey].(string)
	if sid == "" {
		return
	}
	doc, err := o.exportSession(ctx, in, sid)
	if err != nil {
		if in.Console != nil && !in.Probe {
			fmt.Fprintf(in.Console, "\n!! could not read the session export for %s (%v) - usage falls back to the stream's step_finish sums, which miss the final step\n", sid, err)
		}
		return
	}
	applyOpencodeExport(res, doc, started, resume)
}

// exportSession runs `opencode session export --standalone <sid>` with the session's own
// environment and directory, so it reads the same data dir the session wrote.
func (o opencode) exportSession(ctx context.Context, in Invocation, sid string) (opencodeExport, error) {
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opencodeExportTimeout)
	defer cancel()
	cmd, err := sessionCommand(ectx, in.Nice, "opencode", "session", "export", "--standalone", sid)
	if err != nil {
		return opencodeExport{}, err
	}
	setupProcessGroup(cmd)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	env, err := o.env(in)
	if err != nil {
		return opencodeExport{}, err
	}
	cmd.Env = env
	// The standalone server is a grandchild that could hold stdout open; the group kill and WaitDelay
	// keep a stuck export from outliving its bound.
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return cmd.Process.Kill()
	}
	out, err := cmd.Output()
	if err != nil {
		return opencodeExport{}, err
	}
	return parseOpencodeExport(out)
}

// applyOpencodeExport charges res with this run's share of the export (see the attribution note
// above) and recomputes the final-step verdicts from the last assistant message, which the 2.x
// stream does not report.
func applyOpencodeExport(res *Result, doc opencodeExport, started time.Time, resume bool) {
	since := started.UnixMilli()
	var run, all opencodeExportTokens
	var runCost, allCost float64
	var last *opencodeExportMessage
	runMsgs := 0
	for i := range doc.Messages {
		m := &doc.Messages[i]
		if m.Type != "assistant" || m.Tokens == nil {
			continue
		}
		all.add(*m.Tokens, 1)
		allCost += m.Cost
		if m.Time.Created >= since {
			run.add(*m.Tokens, 1)
			runCost += m.Cost
			// A message the provider never answered (a 401 still records one, with no finish and no usage)
			// is not a report.
			if m.Finish != "" || m.Tokens.total() > 0 || m.Cost > 0 {
				runMsgs++
			}
		}
		last = m
	}
	if !resume {
		// Session overhead outside any assistant message: title generation.
		over := *doc.Info.Tokens
		over.add(all, -1)
		if over.total() > 0 {
			run.add(over, 1)
		}
		if c := *doc.Info.Cost - allCost; c > 0 {
			runCost += c
		}
	}

	if res.Raw == nil {
		res.Raw = map[string]any{}
	}
	// Reported means the provider answered at least once in this run (or the session overhead cost
	// something).
	res.UsageReported = res.UsageReported || runMsgs > 0 || run.total() > 0 || runCost > 0
	res.Tokens = run.total()
	res.CostUSD = runCost
	// The export IS the cost figure — parseOpencodeExport refuses a document without info.cost —
	// so this run's dollars are known, even when they are 0.
	res.CostReported = true
	// The normalised breakdown, from the same export figures the budget figures above are computed
	// from — including the session's own overhead (title generation) when this run is not a resume.
	res.Usage = TokenUsage{
		Input:      run.Input,
		Output:     run.Output,
		Reasoning:  run.Reasoning,
		CacheRead:  run.Cache.Read,
		CacheWrite: run.Cache.Write,
	}
	res.Raw["input_tokens"] = run.Input
	res.Raw["output_tokens"] = run.Output
	res.Raw["reasoning_tokens"] = run.Reasoning
	res.Raw["cache_write_tokens"] = run.Cache.Write
	res.Raw["cache_read_tokens"] = run.Cache.Read
	res.Raw[opencodeUsageSourceKey] = opencodeUsageExport

	// The final-step verdicts, only when this run produced the session's last assistant message.
	if last == nil || last.Time.Created < since {
		return
	}
	if last.Finish != "" {
		res.Raw[FinishReasonKey] = last.Finish
	}
	// Our own wall-clock marker is a verdict on how the PROCESS ended and wins; otherwise the two
	// final-step markers are recomputed from the export, which is the only place 2.x reports the final
	// step at all.
	if tr, _ := res.Raw[TerminalReasonKey].(string); tr == wallClockReason {
		return
	}
	delete(res.Raw, TerminalReasonKey)
	delete(res.Raw, OutputCapReasoningKey)
	switch {
	case last.Finish == FinishReasonUnknown && last.Tokens.total() == 0 && last.Cost == 0:
		res.Raw[TerminalReasonKey] = ZeroUsageReason
	case last.Finish == FinishReasonLength && last.Tokens.Output == 0:
		res.Raw[TerminalReasonKey] = OutputCapReason
		res.Raw[OutputCapReasoningKey] = last.Tokens.Reasoning
	}
}
