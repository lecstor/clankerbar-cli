package main

import (
	"encoding/json"
	"fmt"
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

// schemaToolID is the catalog's (unprefixed) name for the schema scenario's
// selected tool: --schema-tool when it is set, otherwise the tool the
// protocol-41 plane serves for the residual.
func schemaToolID(tool string) string {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return "edit_task"
	}
	return strings.TrimPrefix(tool, "clankerbar_")
}

// schemaToolName is the tool name the session sees. The catalog's tools are
// served by the clankerbar MCP server, so opencode exposes `edit_task` as
// `clankerbar_edit_task`; --schema-tool names the catalog's (unprefixed) tool
// and this is the prefixed form the prompt must ask for.
func schemaToolName(tool string) string {
	return "clankerbar_" + schemaToolID(tool)
}

// toolResult is what the stub puts on the wire for one tools/call.
type toolResult struct {
	Text    string
	IsError bool
}

// scenarios is the shipped set. Each one is small on purpose: one question, one
// run, one report.
func scenarios() map[string]scenarioDef {
	// The padding scenario's canned task row starts with both flags SET,
	// because that is the state the residual is about: a padded `false` CLEARS
	// a SET flag, and a `false` on an already-false one is a no-op.
	edit := newEditTaskState()
	return map[string]scenarioDef{
		// padding reproduces strict-mode provider padding against the one
		// strict-mode surface edit_task still has (proposal R4, decision
		// c06cdfc7): a client that fills EVERY optional property with its
		// type's empty value sends `null` on the nullable enums and `false` on
		// the two flags. The prompt therefore asks only for the arguments that
		// DIFFER between the three calls; whether the rest arrives padded is
		// the provider behaviour this scenario exists to observe, not something
		// the prompt should dictate. What the scenario shows:
		//
		//   - `null` on a nullable enum is absence, so a padded call is not
		//     refused for sending one;
		//   - `container` / `humanOnly` take `false` as their CLEAR spelling, so
		//     a padded `false` CLEARS a flag the task had SET - the one place
		//     padding is not safe;
		//   - `repo: ""` clears routing back to the primary repo, while a padded
		//     `repo: null` is absence and leaves it alone;
		//   - a call carrying nothing real is refused `nothing_to_do`, and two
		//     long fields in one call are refused `one_long_field`.
		//
		// The stub answers like the plane does, including the `changed` receipt,
		// and the report lists the keys sent per call plus the residual signals
		// each call actually carried.
		"padding": {
			Name: "padding",
			Prompt: strings.TrimSpace(`
Call the tool clankerbar_edit_task three times, one call at a time, and pass
taskId "CLA-753" every time. The calls differ only in these arguments:

1. detail: "the new body"
2. repo: ""
3. repo: null

Then answer with one sentence saying what each call reported changed, and stop.`),
			Respond: func(tool string, args map[string]any) toolResult {
				if tool != "edit_task" {
					return defaultToolResult(tool)
				}
				return editTaskReceipt(edit, args)
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
		// selected tool is the one actually exercised, and it asks for exactly
		// the shape a nullable enum is easy to get wrong on: a null everywhere
		// the schema allows one.
		"schema": {
			Name: "schema",
			PromptFor: func(schemaTool string) string {
				name := schemaToolName(schemaTool)
				return strings.TrimSpace(fmt.Sprintf(`
Call the tool %s exactly once. Pass taskId "CLA-753" if its schema declares one, send null for every optional argument its schema allows to be null, and supply the arguments its schema requires. Then say DONE and stop.`, name))
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

// catalogTool returns the tool a scenario's stub answers and its prompt asks
// for, when the scenario is pointless without that tool in the catalog the
// session is offered. "" means the scenario does not depend on a catalog tool.
func catalogTool(scenario, schemaTool string) string {
	switch scenario {
	case "schema":
		return schemaToolID(schemaTool)
	case "padding":
		// The residual lives on edit_task. A catalog that predates the tool
		// cutover does not serve it, and the model would be offered everything
		// except the surface this scenario studies.
		return "edit_task"
	case "reasoning-replay":
		// Both tool turns ask for get_task and the stub answers it; a catalog
		// without it produces the same pointless run as the padding scenario
		// against a pre-cutover catalog.
		return "get_task"
	}
	return ""
}

// editTaskState is the canned task row the padding scenario edits, reduced to
// the two flags whose CURRENT value decides whether a padded `false` is a CLEAR
// or a no-op. Both start SET, so the residual is observable on the first call.
type editTaskState struct {
	container bool
	humanOnly bool
}

func newEditTaskState() *editTaskState {
	return &editTaskState{container: true, humanOnly: true}
}

// editTaskFields is the order the plane's edit_task builds its revision in, so
// the receipt's `changed` reads the same here as it does from the plane.
var editTaskFields = []string{
	"title", "detail", "doneWhen", "category", "repo",
	"priority", "schedule", "container", "humanOnly",
}

// editTaskReceipt answers one edit_task call the way the protocol-41 plane
// does, reduced to what this scenario exercises. The order is the plane's:
// empty arguments are dropped first (an empty value is an absent one), then
// edit_task's own guards run (taskId required, at most one long field), then a
// `false` on an already-false flag is dropped so the receipt never claims a
// change the call did not make.
func editTaskReceipt(st *editTaskState, args map[string]any) toolResult {
	sent := dropEmptyArgs(args)
	if taskID, _ := sent["taskId"].(string); taskID == "" {
		return toolError("missing_argument",
			"Required argument(s) absent or empty: taskId. `edit_task` needs the task to "+
				"revise. Nothing was written.")
	}
	if longs := longFieldsCarried(sent); len(longs) > 1 {
		return toolError("one_long_field",
			"`edit_task` takes at most ONE long field per call, and this call carries "+
				strings.Join(longs, " and ")+
				". The emission defect that swallows a parameter merges ADJACENT long ones, "+
				"so two in one call can silently absorb each other - send them as separate "+
				"calls. Nothing was written.")
	}

	changed := make([]string, 0, len(editTaskFields))
	for _, field := range editTaskFields {
		v, ok := sent[field]
		if !ok {
			continue
		}
		if field == "container" || field == "humanOnly" {
			set, _ := v.(bool)
			current := st.container
			if field == "humanOnly" {
				current = st.humanOnly
			}
			// A padded `false` on an already-false flag is a no-op (CLA-745),
			// and a receipt listing it would claim a change nobody made. On a
			// SET flag the `false` IS the clear - the ruled residual.
			if !set && !current {
				continue
			}
			if field == "container" {
				st.container = set
			} else {
				st.humanOnly = set
			}
		}
		changed = append(changed, field)
	}
	if len(changed) == 0 {
		return toolError("nothing_to_do",
			"`edit_task` needs at least one field to revise. Nothing was written.")
	}
	return jsonResult(map[string]any{
		"task": map[string]any{
			"id":     "00000000-0000-4000-8000-000000000753",
			"ref":    "CLA-753",
			"status": "backlog",
		},
		"changed": changed,
		"note":    "rig stub: accepted, nothing written",
	})
}

// dropEmptyArgs mirrors the plane's rule that an EMPTY VALUE IS AN ABSENT ONE
// (tool-args.ts): `null` is absence on every property, `""` is absence on the
// text fields EXCEPT `repo` whose `""` is the documented CLEAR, and an empty
// list is absence. It deliberately KEEPS a `false` on container/humanOnly,
// which is their CLEAR spelling - the ruled residual this scenario exists to
// show.
func dropEmptyArgs(args map[string]any) map[string]any {
	textFields := map[string]bool{
		"title": true, "detail": true, "doneWhen": true, "category": true,
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" && textFields[k] {
			continue
		}
		if xs, ok := v.([]any); ok && len(xs) == 0 {
			continue
		}
		out[k] = v
	}
	return out
}

// longFieldsCarried returns the long (prose) fields a call actually carries.
// edit_task refuses more than one per call: the emission defect merges ADJACENT
// long parameters, so the loss is schema-valid and nothing else catches it.
func longFieldsCarried(sent map[string]any) []string {
	var out []string
	for _, field := range []string{"title", "detail", "doneWhen"} {
		if s, ok := sent[field].(string); ok && s != "" {
			out = append(out, field)
		}
	}
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
