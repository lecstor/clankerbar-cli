package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// scenarioDef is one named experiment: the prompt handed to the model and the
// canned MCP results the stub serves while it runs.
//
// A scenario never talks to the plane. Its canned results exist so the session
// reaches the model/provider behaviour under test and stops; what the run
// proves is in the captured requests and the stub's call log, not in any write.
type scenarioDef struct {
	Name string
	// Prompt is the message `opencode run` sends. It is empty when PromptFor
	// builds it from the run's flags instead.
	Prompt string
	// PromptFor builds the prompt from the run's selection, for a scenario whose
	// prompt names a flag-chosen tool (the schema scenario's --schema-tool).
	PromptFor func(schemaTool string) string
	// Respond decides the MCP tool result for one call. A nil Respond means the
	// stub's default ("no canned response"), which is enough for a scenario
	// that only wants the call captured.
	Respond func(tool string, args map[string]any) toolResult
}

// promptFor resolves the scenario's prompt for these flags.
func (s scenarioDef) promptFor(schemaTool string) string {
	if s.PromptFor != nil {
		return s.PromptFor(schemaTool)
	}
	return s.Prompt
}

// defaultSchemaTool is the tool the schema scenario probes when --schema-tool
// is not given. It must be a tool this release's plane serves (CLA-748): the
// cutover deletes update_task, so a probe defaulting to it would exercise a
// name the plane answers as unknown and prove nothing about a real schema.
const defaultSchemaTool = "record_progress"

// schemaToolName is the tool name the session sees. The catalog's tools are
// served by the clankerbar MCP server, so opencode exposes `record_progress` as
// `clankerbar_record_progress`; --schema-tool names the catalog's (unprefixed)
// tool and this is the prefixed form the prompt must ask for.
func schemaToolName(tool string) string {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		tool = defaultSchemaTool
	}
	if strings.HasPrefix(tool, "clankerbar_") {
		return tool
	}
	return "clankerbar_" + tool
}

// toolResult is what the stub puts on the wire for one tools/call.
type toolResult struct {
	Text    string
	IsError bool
}

// scenarios is the shipped set. Each one is small on purpose: one question, one
// run, one report.
func scenarios() map[string]scenarioDef {
	return map[string]scenarioDef{
		// padding reproduces the provider padding a review-shaped update_task
		// with every nullable property set to null, which the plane refuses as
		// invalid_review. The refusal names the offenders the arguments actually
		// carried, exactly as the plane's reviewCallRefusal does, and the report
		// lists the keys sent per call.
		"padding": {
			Name: "padding",
			Prompt: strings.TrimSpace(`
Call the tool clankerbar_update_task once to record this review:
- taskId: CLA-753
- runId: rig-run-1
- review: reviewerRole "review-1", verdict "pass"
Then answer with one sentence summarizing what the tool returned and stop.`),
			Respond: func(tool string, args map[string]any) toolResult {
				if tool != "update_task" {
					return defaultToolResult(tool)
				}
				offenders := reviewPadding(args)
				if len(offenders) == 0 {
					// The plane would write the record; the stub never writes.
					return toolResult{Text: `{"ok":true,"note":"rig stub: review accepted, nothing written"}`}
				}
				msg := "a review call carries only taskId, runId and review - resend it without: " +
					strings.Join(offenders, ", ") +
					". Those fields would change the task, and a reviewer records its review and " +
					"nothing else. Nothing was written."
				return toolError("invalid_review", msg)
			},
		},

		// reasoning-replay asks for two tool turns. On the second request the
		// assistant message that carried the first call is replayed by opencode;
		// whether it still carries reasoning_content is the question, and the
		// report prints the assistant messages' extra fields per request.
		"reasoning-replay": {
			Name: "reasoning-replay",
			Prompt: strings.TrimSpace(`
Call the tool clankerbar_get_task once for CLA-753. When it returns, call
clankerbar_get_task once for CLA-754. Then say DONE and stop.`),
			Respond: func(tool string, args map[string]any) toolResult {
				if tool != "get_task" {
					return defaultToolResult(tool)
				}
				taskID, _ := args["taskId"].(string)
				if taskID == "" {
					taskID = "CLA-753"
				}
				payload := map[string]any{
					"task": map[string]any{
						"id":       "00000000-0000-4000-8000-000000000753",
						"ref":      taskID,
						"title":    "rig fixture task",
						"status":   "ready",
						"priority": "m",
						"detail":   "A canned task served by the opencode rig. Nothing here is real backlog state.",
					},
					"decisionsAvailable": map[string]any{"total": 0},
				}
				return jsonResult(payload)
			},
		},

		// schema serves whatever catalog --schemas names, so a proposed tool
		// shape (nullable enums, a new tool) can be tested before the plane
		// ships it. The prompt names the tool --schema-tool selects, so the
		// selected tool is the one actually exercised: it asks for null on every
		// optional argument the schema allows, the padding a strict-mode client
		// is easy to get wrong on.
		"schema": {
			Name: "schema",
			PromptFor: func(schemaTool string) string {
				name := schemaToolName(schemaTool)
				return strings.TrimSpace(fmt.Sprintf(`
Call the tool %s exactly once, sending null for every optional argument its schema allows to be null, and the arguments its schema requires. Then say DONE and stop.`, name))
			},
			Respond: func(tool string, args map[string]any) toolResult {
				return jsonResult(map[string]any{
					"ok":   true,
					"note": "rig stub: schema call accepted, nothing written",
					"echo": args,
				})
			},
		},
	}
}

// reviewPadding returns the review call's own top-level arguments that the
// plane's review rule refuses, sorted. It mirrors the plane: a review call may
// carry taskId, runId and review and nothing else, and a padded nullable
// property sent as null is still an argument that was passed.
func reviewPadding(args map[string]any) []string {
	allowed := map[string]bool{"taskId": true, "runId": true, "review": true}
	var out []string
	for k := range args {
		if !allowed[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// toolError renders the plane's structured tool failure: a JSON text payload
// with error.code and error.message, and isError true.
func toolError(code, message string) toolResult {
	res := jsonResult(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
	res.IsError = true
	return res
}

// jsonResult renders a successful tool result carrying JSON text, the shape the
// plane uses for every tool that returns data.
func jsonResult(v any) toolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return toolResult{Text: fmt.Sprintf(`{"error":{"code":"internal_error","message":%q}}`, err.Error()), IsError: true}
	}
	return toolResult{Text: string(b)}
}

// defaultToolResult answers a tool the scenario did not script. It is a
// successful, explicit no-op so a curious model is not left retrying an error
// that the rig never intended to test.
func defaultToolResult(tool string) toolResult {
	return jsonResult(map[string]any{
		"ok":   true,
		"note": "rig stub: no canned response for " + tool + "; nothing written",
	})
}
