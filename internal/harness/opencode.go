package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() { Register(opencode{}) }

// opencode drives the open, provider-agnostic opencode CLI, 2.x line (`opencode run`).
// `--standalone` is load-bearing: 2.x is client/server and `run` otherwise attaches to the managed
// background service, whose own config then wins over everything this adapter sets.
type opencode struct{}

func (opencode) Name() string { return "opencode" }

// opencode DOES receive mcp_config_path - as OPENCODE_CONFIG and, rewritten, as
// OPENCODE_CONFIG_CONTENT (see env below) - and that is exactly why "does the adapter hand it over"
// was the wrong question to gate a preflight on. A Claude-shaped file is a different schema: 1.18.2
// refused to start on it, and 1.18.18 and 2.0.32 silently skip its `mcpServers` key, so the session
// gets no clankerbar tools.
func (opencode) MCPConfigUse() MCPConfigUse {
	return MCPConfigUse{
		Schema: MCPConfigNative,
		Note: "the opencode adapter passes mcp_config_path as OPENCODE_CONFIG (plus a rewritten copy as OPENCODE_CONFIG_CONTENT), which must be an opencode " +
			"config (servers under `mcp` or `mcp.servers`, not Claude's `mcpServers`) - opencode skips a key it does not know, so a Claude-shaped file gives sessions no clankerbar tools",
	}
}

// Invoke runs one opencode session for the driver, and — CLA-406 — when that session dies the
// quiet death (stream dropped mid-generation, no terminal chunk, no usage), tries to RESURRECT it
// in place rather than hand the driver a corpse: back off, probe the session with a coherence
// question, and on a mechanical match continue it where it left off. See opencode_resume.go for the
// rules and their rationale; the loop here is only their orchestration.
func (o opencode) Invoke(ctx context.Context, in Invocation) (Result, error) {
	// Console -> agent delivery. The RUN id is not known at spawn for a fresh claim — the session
	// claims inside itself — so the adapter learns it from ResumeClaim (a resumed phase) or the
	// OnClaim watcher, exactly as claude does.
	var delivery *opencodeInbox
	if in.Inbox != nil && !in.Probe {
		runIDs := make(chan string, 8)
		if in.ResumeClaim.RunID != "" {
			runIDs <- in.ResumeClaim.RunID
		}
		lastRunID := in.ResumeClaim.RunID
		origOnClaim := in.OnClaim
		in.OnClaim = func(c Claim) {
			// A settle fires this callback with the run id unchanged; only a NEW run is a delivery target.
			if c.RunID != "" && c.RunID != lastRunID {
				lastRunID = c.RunID
				select {
				case runIDs <- c.RunID:
				default: // pathological churn; the newest claim will start the next poller
				}
			}
			if origOnClaim != nil {
				origOnClaim(c)
			}
		}
		delivery = newOpencodeInbox(ctx, in.Console, *in.Inbox, runIDs)
		defer delivery.close()
	}

	// The wall-clock cap bounds the WHOLE Invoke, not each process: the deadline is computed BEFORE
	// the first run so the original run's time counts against the budget the continuations inherit.
	var deadline time.Time
	if in.MaxSessionWallClock > 0 {
		deadline = time.Now().Add(in.MaxSessionWallClock)
	}
	res, err := o.runSession(ctx, in, opencodeArgs(in), delivery)
	if err != nil {
		return res, err
	}
	if in.Probe {
		// Never resurrect a probe: a probe IS the liveness check.
		return res, nil
	}
	if len(in.Turns) > 0 {
		// A planned multi-turn session: the follow-up turns run in this same session before any recovery
		// pass, because a turn that ends badly must stop the sequence rather than be resumed behind the
		// caller's back. A completed sequence hands back the FINAL turn's invocation: everything below
		// judges the turn that actually ended the session, never the first one's model or route list.
		var completed bool
		in, completed, err = o.runTurns(ctx, in, &res, deadline, delivery)
		if err != nil {
			return res, err
		}
		if !completed {
			return res, nil
		}
	}
	if res.ToolLoop != "" || res.IdleKilled != "" {
		// CLA-739 / the idle watchdog: killed for a cause of the adapter's own.
		return res, nil
	}
	console := in.Console
	// CLA-584: the output-cap stall. A final step that hit the output token limit with zero output is
	// NOT the quiet death the loop below resumes — the session is alive and its transcript is
	// intact; the step failed because the model spent its whole output budget thinking.
	if o.OutputCapNoOutput(res) {
		o.resumeOutputCapStall(ctx, in, &res, deadline, delivery)
	}
	o.resumeInterrupted(ctx, in, &res, deadline, delivery)
	if !in.Probe {
		if err := o.switchRoutesOnLimit(ctx, in, in.RouteList(), &res, deadline, delivery); err != nil {
			return res, err
		}
		// Keep later resumes on the route that actually ran: the resurrection probe and continuation
		// below read in.ModelArg(), and resuming on the route that was just denied would only re-hit its
		// limit.
		if finished := RouteFinished(res); finished != "" {
			in = in.OnRoute(finished)
		}
	}
	// An UNTRUSTED stream is never resurrected: CLA-262 forbids reading claim state off an over-run
	// capture, and both the coherence ref and the continuation's seed come from exactly that state -
	// while the driver's own untrusted path declines to count further spend, so probing would enlarge
	// the accounting blind spot endUntrustedDrain exists to bound.
	for attempt := 1; attempt <= opencodeMaxResurrections && o.ZeroUsageUnknown(res) && res.Untrusted == "" && res.ToolLoop == ""; attempt++ {
		sid, ref := resumeTargets(res)
		if sid == "" || ref == "" {
			break
		}
		// Budget check BEFORE the round spends anything: a round's backoff plus probe box come off the
		// clock before any continuation could, so a slice under the round floor can only buy a probe
		// whose answer is never acted on.
		if !deadline.IsZero() && time.Until(deadline) < opencodeResumeRoundFloor {
			if console != nil {
				fmt.Fprint(console, opencodeWallClockExhaustedMsg)
			}
			break
		}
		if console != nil {
			fmt.Fprintf(console, "\n!! quiet death detected — resurrecting session %s (attempt %d of %d): backing off %s, then probing for coherence against %s\n",
				sid, attempt, opencodeMaxResurrections, opencodeResumeBackoff, ref)
		}
		select {
		case <-ctx.Done():
			// Run-wide cancellation (Ctrl-C / supervised wait): not this phase's verdict.
			return res, nil
		case <-time.After(opencodeResumeBackoff):
		}

		pctx, pcancel := context.WithTimeout(ctx, opencodeProbeTimeout)
		probeIn := in
		probeIn.Probe = true // no wall-clock double-cap, no console tee
		probeIn.ResumeClaim = res.Claim
		probeRes, probeErr := o.runSession(pctx, probeIn,
			opencodeResumeArgs(in, sid, resurrectionProbePrompt), nil) // a probe has no private server to steer
		pcancel()
		res.Tokens += probeRes.Tokens
		res.CostUSD += probeRes.CostUSD
		res.Usage.Add(probeRes.Usage)
		res.UsageReported = res.UsageReported || probeRes.UsageReported
		res.CostReported = res.CostReported || probeRes.CostReported
		// The claim observation folds ONLY when it is still about THIS task and arrived on a capture we
		// may read: noteClaimed replaces a parser's claim WHOLESALE, so a decohered probe that re-claimed
		// a different task would otherwise redirect the merged Result's identity — the real lease
		// expiring while release/salvage/tally all fire against a task the dead session never held.
		if probeRes.Untrusted == "" && probeRes.Claim.Names(res.Claim.TaskID) {
			mergeObservation(&res, probeRes)
		}
		coherent := probeErr == nil && probeRes.Untrusted == "" && probeCoherent(probeRes.FinalMessage, ref)
		if !coherent {
			if console != nil {
				fmt.Fprintf(console, "!! resurrection probe FAILED (err=%v, reply=%.200q) — counting the death\n",
					probeErr, probeRes.FinalMessage)
			}
			break
		}

		// Coherent: continue the session for real. The continuation's parser is seeded with the DEAD
		// RUN'S live claim (not in.ResumeClaim, which on a phase 2+ holds the staler predecessor):
		// `opencode run -s` streams only the new turn, so without the seed every progress write the
		// continuation makes would go unobserved — no settle, no HasWIP, no delivery report — and
		// releaseHeldClaim would post ready over work the session pushed.
		contIn := in
		contIn.ResumeClaim = res.Claim
		if !deadline.IsZero() {
			// Re-checked here, not just at the top of the round: the probe just consumed up to its own
			// time-box, and a continuation that could only be cap-killed on arrival must not be started.
			remaining := time.Until(deadline)
			if remaining < opencodeResumeWallClockFloor {
				if console != nil {
					fmt.Fprint(console, opencodeWallClockExhaustedMsg)
				}
				break
			}
			contIn.MaxSessionWallClock = remaining
		}
		contRes, contErr := o.runSession(ctx, contIn,
			opencodeResumeArgs(in, sid, continuePrompt), delivery)
		if contErr != nil {
			// Spend first, even on the error path.
			res.Tokens += contRes.Tokens
			res.CostUSD += contRes.CostUSD
			res.Usage.Add(contRes.Usage)
			res.UsageReported = res.UsageReported || contRes.UsageReported
			res.CostReported = res.CostReported || contRes.CostReported
			if console != nil {
				fmt.Fprintf(console, "!! resurrection continuation errored (%v) — counting the death\n", contErr)
			}
			break
		}
		n := 0
		if nRaw, ok := res.Raw[opencodeResurrectionsKey].(int); ok {
			n = nRaw
		}
		mergeResume(&res, contRes)
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[opencodeResurrectionsKey] = n + 1
		if console != nil {
			fmt.Fprintf(console, "!! resurrection %d succeeded — session continued in place\n", n+1)
		}
	}
	o.resumeBadReport(ctx, in, &res, deadline, delivery)
	if len(res.TurnMessages) > 0 {
		res.TurnMessages[len(res.TurnMessages)-1] = res.FinalMessage
	}
	// Studio chat (studio-chat.md §3): a stream that never named a session still ran under the handle
	// the invocation gave it, and that is what the planner records.
	if res.SessionRef == "" {
		res.SessionRef = strings.TrimSpace(in.SessionRef)
	}
	return res, nil
}

// opencodeArgs maps an Invocation to `opencode run`'s CLI dialect. Split out so the probe's
// read-only shape is unit-testable without executing anything.
func opencodeArgs(in Invocation) []string {
	prompt := opencodePrompt(in.Prompt)
	if in.Probe {
		// The cheapest possible request: a trivial prompt, tools all denied by the read-only permission
		// policy (see env) — just enough to see whether the provider still answers or is
		// budget-exhausted.
		prompt = "."
	}
	args := []string{"run", "--standalone", "--format", "json", "--agent", opencodeAgent}
	if m := in.ModelArg(); m != "" {
		args = append(args, "--model", m)
	}
	if ref := strings.TrimSpace(in.SessionRef); ref != "" {
		args = append(args, "-s", ref)
	}
	return append(args, "--", prompt)
}

// opencodePrompt is the message a working (non-probe) run sends. CLA-703 removed the race that note
// described: the adapter now starts its own server, waits until the clankerbar server reports
// connected, and only then attaches the run, so the first model turn already has the tools.
func opencodePrompt(prompt string) string {
	return prompt
}

// opencodeMCPResourcePattern is the pattern shape opencode's MCP resource tools ask with under the
// `read` permission: "mcp:<server>:<uri>" for a read and "mcp:<server>:*" for a listing.
const opencodeMCPResourcePattern = "mcp:*"

// opencodePermission is the fail-closed policy the adapter writes into the `permission` key of the
// config it pins via OPENCODE_CONFIG_CONTENT (see opencodeSessionConfig): a path-scoped
// PermissionConfig, not a flat tool map. Everything else fails closed via the `*` catch-all, and
// the network/exfil tools (webfetch/websearch) are denied in both shapes.
func opencodePermission(readOnly bool, workdir string, extraDirs []string) string {
	b, _ := json.Marshal(opencodePermissionPolicy(readOnly, workdir, extraDirs))
	return string(b)
}

// opencodePermissionPolicy is opencodePermission before marshalling, so the session config can
// embed it as an object rather than a string.
func opencodePermissionPolicy(readOnly bool, workdir string, extraDirs []string) map[string]any {
	_, abs := opencodeWorkdirPatterns(workdir)
	read := map[string]string{"**": "allow", "../**": "deny", opencodeMCPResourcePattern: "allow"}
	edit := map[string]string{"**": "allow", "../**": "deny"}
	ext := map[string]string{"*": "deny", abs: "allow"}
	scopeExtraDirs(read, edit, ext, opencodeInstanceDir(workdir), extraDirs)
	perm := map[string]any{
		"read":               read,
		"edit":               edit,
		"external_directory": ext,
		// MCP tools must survive the `*` catch-all — see the function doc.
		"*_*": "allow",
		// The read-only search tools: grep/glob/list. Their asks carry the search pattern (a regex, a
		// glob, or the resolved directory), never a workdir-relative path, so a path-scoped rule could
		// not match them and the catch-all would leave the tools denied — the CLA-390 bug.
		"grep": map[string]string{"**": "allow"},
		"glob": map[string]string{"**": "allow"},
		"list": map[string]string{"**": "allow"},
		// The exfil guards, explicit in both shapes.
		"webfetch":  "deny",
		"websearch": "deny",
	}
	// Code Mode, denied by name: see the function doc.
	perm["execute"] = "deny"
	perm["opencode_*"] = "deny"
	perm["opencode_list_mcp_resources"] = "allow"
	perm["opencode_read_mcp_resource"] = "allow"
	if readOnly {
		perm["edit"] = "deny"
		perm["shell"] = "deny"
	} else {
		perm["shell"] = "allow"
	}
	perm["*"] = "deny"
	return perm
}

// scopeExtraDirs widens the policy's three path-scoped rules to cover extraDirs: the absolute
// external_directory allow, and the instance-relative read/edit form. Emitting it would either
// overwrite the "../**" deny with an allow or sort after it and beat it ("../../**"), turning the
// policy's only stated escape boundary into an allow of the whole filesystem.
func scopeExtraDirs(read, edit, ext map[string]string, base string, extraDirs []string) {
	for _, dir := range extraDirs {
		if dir == "" {
			continue
		}
		_, abs := opencodeWorkdirPatterns(dir)
		ext[abs] = "allow"
		rel, err := filepath.Rel(base, absOrSelf(dir))
		if err != nil || rel == "." || isDotDotChain(rel) || !strings.HasPrefix(filepath.ToSlash(rel), "../") {
			continue
		}
		relGlob := filepath.ToSlash(rel)
		read[relGlob+"/**"] = "allow"
		edit[relGlob+"/**"] = "allow"
		read[relGlob] = "allow"
		edit[relGlob] = "allow"
	}
}

// isDotDotChain reports whether a slash-separated relative path is nothing but ".." components -
// what filepath.Rel returns for a directory that CONTAINS the base.
func isDotDotChain(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg != ".." {
			return false
		}
	}
	return true
}

// opencodeInstanceDir is the directory a session's asks are relative to: the workdir, or the
// process cwd when the workdir is empty (where the session would actually run).
func opencodeInstanceDir(workdir string) string {
	if workdir == "" {
		if cwd, err := os.Getwd(); err == nil {
			return cwd
		}
	}
	return absOrSelf(workdir)
}

func absOrSelf(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// opencodeWorkdirPatterns derives two pattern forms from a directory. rootRel is the root-relative
// form 1.x read/edit asks used (the absolute path minus its leading separator); 2.x never asks in
// it, and the policy no longer emits it, but the helper keeps returning it for the tests that pin
// its derivation.
func opencodeWorkdirPatterns(workdir string) (rootRel, abs string) {
	if workdir == "" {
		var err error
		if workdir, err = os.Getwd(); err != nil {
			// Unresolvable cwd: match NOTHING rather than everything — the policy's stated posture is
			// fail-closed.
			return "/**", "\x00/**"
		}
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return "/**", "\x00/**"
	}
	abs = filepath.Clean(abs)
	vol := filepath.VolumeName(abs)
	rel := strings.TrimPrefix(abs, vol)
	rel = strings.TrimLeft(rel, `/\`)
	return filepath.ToSlash(rel) + "/**", filepath.ToSlash(abs) + "/**"
}

// withoutAmbientOpencodeConfig drops an inherited OPENCODE_CONFIG: the driver decides which file a
// session is handed, and an ambient value would hand one over even when config resolved none.
func withoutAmbientOpencodeConfig(env []string) []string {
	return withoutEnvKey(env, "OPENCODE_CONFIG")
}

// withoutEnvKey drops every assignment of one variable.
func withoutEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (opencode) env(in Invocation) ([]string, error) {
	// The session's INSTANCE directory.
	env := pinPWD(inheritEnv(withoutAmbientOpencodeConfig(os.Environ()), in.EnvAllowlist), in.WorkDir)
	if in.ConfigDir != "" {
		env = append(env, "OPENCODE_CONFIG_DIR="+in.ConfigDir)
	}
	if in.MCPConfigPath != "" {
		env = append(env, "OPENCODE_CONFIG="+in.MCPConfigPath)
	}
	content, err := opencodeSessionConfigWithEnv(in, env)
	if err != nil {
		// FAIL CLOSED. The alternative - spawn without the content layer - is a session with no
		// permission policy at all on 2.x, claiming from whichever project the ambient config names.
		return nil, err
	}
	env = append(env, "OPENCODE_CONFIG_CONTENT="+string(content))
	env = append(env, in.Env...)
	return env, nil
}

// opencodeSessionConfig is opencodeSessionConfigWithEnv with no environment: the
// path doctor and the unit tests use, and never a NoBacklogMCP session.
func opencodeSessionConfig(in Invocation) ([]byte, error) {
	return opencodeSessionConfigWithEnv(in, nil)
}

// opencodeSessionConfigWithEnv builds the document pinned as
// OPENCODE_CONFIG_CONTENT: the mcp config file's own keys, with every MCP server
// moved out of Code Mode and the fail-closed permission policy under
// `permission`.
//
// env is the environment built so far (before the content layer is appended), so
// the NoBacklogMCP switch can resolve the same ambient layers opencode will load.
//
// The file is parsed as strict JSON, the same rule config applies to every file
// the driver names (readMCPFile): those feed fail-closed gates, and a lenient
// parse here would let the adapter accept what config refused. Numbers are kept
// as written (UseNumber) so a re-marshalled timeout or port is byte-for-byte the
// operator's value.
//
// The file's own `permission` key, if any, is replaced here rather than merged.
// That does not neutralise it: the same file is also loaded as OPENCODE_CONFIG,
// an earlier layer, and opencode merges permission blocks key by key in
// insertion order, so a key the file declares sorts ahead of this policy's `*`
// catch-all. doctor WARNs on a named file carrying one (and config refuses one
// in a DISCOVERED file, checkDiscoveredMCPConfig).
func opencodeSessionConfigWithEnv(in Invocation, env []string) ([]byte, error) {
	if in.NoBacklogMCP {
		// A declared OPENCODE_CONFIG_CONTENT replaces this document wholesale
		// (exec takes the last of a duplicate key), and with it the backlog
		// switch. Fail closed rather than hand a standalone review the tools it
		// exists to avoid.
		for _, kv := range in.Env {
			if strings.HasPrefix(kv, "OPENCODE_CONFIG_CONTENT=") {
				return nil, errors.New("a no-backlog session cannot run with a declared OPENCODE_CONFIG_CONTENT: it replaces the adapter's content layer, and with it the switch that keeps the backlog MCP server off")
			}
		}
		// The switch resolves the layers against the environment opencode will
		// SEE, so a caller-declared OPENCODE_CONFIG_DIR or OPENCODE_CONFIG (which
		// exec lets win) cannot name a layer the resolver never read. An
		// INHERITED content layer is dropped: this call is about to write the
		// one that replaces it.
		env = append(withoutEnvKey(env, "OPENCODE_CONFIG_CONTENT"), in.Env...)
	}
	doc := map[string]any{}
	if in.MCPConfigPath != "" {
		data, err := os.ReadFile(in.MCPConfigPath)
		if err != nil {
			return nil, fmt.Errorf("mcp config %s: %w", in.MCPConfigPath, err)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("mcp config %s: not a JSON object: %w", in.MCPConfigPath, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	}
	// CLA-927: a standalone session that must not reach a backlog gets every
	// clankerbar server opencode would load switched off in THIS layer, which
	// opencode merges last. Without it a checkout's checked-in opencode.json
	// hands the review the operator's backlog tools (with a key in env) or fails
	// the preflight with needs_auth (without one).
	if in.NoBacklogMCP {
		decls, err := opencodeBacklogMCPDeclarations(in, env, true)
		if err != nil {
			return nil, err
		}
		disableBacklogMCPServers(doc, decls)
	}
	if raw, ok := doc["mcp"]; ok && raw != nil {
		source := "backlog MCP disable"
		if in.MCPConfigPath != "" {
			source = "mcp config " + in.MCPConfigPath
		}
		if err := opencodeMCPOutOfCodeMode(doc); err != nil {
			return nil, fmt.Errorf("%s: %w", source, err)
		}
	}
	policy := opencodePermissionPolicy(in.Probe, in.WorkDir, in.ExtraDirs)
	doc["permission"] = policy
	// Pin the same policy on every built-in agent.
	agents := map[string]any{}
	for _, name := range opencodePinnedAgents {
		agents[name] = map[string]any{"permission": policy}
	}
	doc["agent"] = agents
	delete(doc, "agents")
	delete(doc, "default_agent")
	// CLA-877: a hung provider stream must ERROR, not stay silent until the phase idle watchdog kills
	// the whole session.
	opencodeProviderTimeouts(in, doc)
	return json.Marshal(doc)
}

// OpencodeSessionConfig is the document the opencode adapter pins as OPENCODE_CONFIG_CONTENT for a
// session with these inputs.
func OpencodeSessionConfig(mcpConfigPath, workdir string, readOnly bool) ([]byte, error) {
	return opencodeSessionConfig(Invocation{MCPConfigPath: mcpConfigPath, WorkDir: workdir, Probe: readOnly})
}

// opencodePinnedAgents are the agents the session config pins the policy on: opencodeAgent, which
// every run selects explicitly, and the other agents 2.0.32 defines. The session's own policy
// denies the subagent tool, so the others are pinned as a second line, not because a session can
// reach them.
var opencodePinnedAgents = []string{opencodeAgent, "plan", "general", "explore"}

// opencodeAgent is the agent every run selects with --agent. Selecting it explicitly means an
// ambient `default_agent` cannot route the session to an agent whose permissions this adapter never
// pinned.
const opencodeAgent = "build"

// opencodeMCPOutOfCodeMode rewrites a config's `mcp` block in place so every server it declares is
// in the 2.x dialect (`mcp.servers.<name>`) with `codemode: false`. Why both halves are needed,
// verified against 2.0.32: - With Code Mode on (the 2.x default) a server's tools are reachable
// only from inside the `execute` tool, which this adapter denies, and whose stream would not show
// the claim anyway.
func opencodeMCPOutOfCodeMode(doc map[string]any) error {
	raw, ok := doc["mcp"]
	if !ok || raw == nil {
		return nil
	}
	mcp, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("`mcp` is not an object")
	}
	servers := map[string]any{}
	if rs, present := mcp["servers"]; present {
		s, ok := rs.(map[string]any)
		if !ok {
			return fmt.Errorf("`mcp.servers` is not an object")
		}
		servers = s
	}
	for name, v := range mcp {
		if name == "servers" {
			continue
		}
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if _, typed := entry["type"].(string); !typed {
			continue
		}
		if _, dup := servers[name]; dup {
			return fmt.Errorf("MCP server %q is declared both as `mcp.%s` and `mcp.servers.%s`", name, name, name)
		}
		if enabled, isBool := entry["enabled"].(bool); isBool {
			delete(entry, "enabled")
			if !enabled {
				entry["disabled"] = true
			}
		}
		servers[name] = entry
		delete(mcp, name)
	}
	for name, v := range servers {
		entry, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("`mcp.servers.%s` is not an object", name)
		}
		entry["codemode"] = false
	}
	mcp["servers"] = servers
	return nil
}

// opencodeTruncatedOutput returns the full text of a tool result 2.x truncated (see
// opencodeToolState), ok=false when the result was not truncated. Any failure is returned as an
// error; the caller fails safe.
func opencodeTruncatedOutput(st *opencodeToolState, dir string) (string, bool, error) {
	if st.Metadata == nil || st.Metadata.Metadata == nil || !st.Metadata.Metadata.Truncated {
		return "", false, nil
	}
	path := st.Metadata.Metadata.OutputPath
	if path == "" || !filepath.IsAbs(path) {
		return "", false, fmt.Errorf("no absolute outputPath in the truncation metadata (%q)", path)
	}
	if dir == "" {
		return "", false, fmt.Errorf("the session's tool-output directory is unknown")
	}
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false, fmt.Errorf("tool-output directory %s: %w", dir, err)
	}
	gotDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || gotDir != wantDir {
		return "", false, fmt.Errorf("%s is not in this session's tool-output directory %s", path, dir)
	}
	if !strings.HasPrefix(filepath.Base(path), "tool_") {
		return "", false, fmt.Errorf("%s is not an opencode tool-output file", path)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", false, err
	}
	if !fi.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := readSpilled(path)
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// opencodeToolOutputDir is <data home>/opencode/tool-output for the session in: XDG_DATA_HOME, else
// HOME/.local/share, each read from the session's own environment (in.Env wins over the inherited
// one, as it does on exec).
func opencodeToolOutputDir(in Invocation) string {
	get := func(k string) string {
		v := os.Getenv(k)
		for _, kv := range in.Env {
			if name, val, ok := strings.Cut(kv, "="); ok && name == k {
				v = val
			}
		}
		return v
	}
	data := get("XDG_DATA_HOME")
	if data == "" {
		home := get("HOME")
		if home == "" {
			return ""
		}
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "opencode", "tool-output")
}

// opencodeClankerbarTool maps an opencode tool name onto the namespaced name the shared observer
// switches on, and returns "" for anything that is not a clankerbar tool. The server name
// `clankerbar` is HARD-CODED, here as in claude's constants, and that is an assumption the driver
// does not enforce: it never writes an MCP config (the operator hand-writes opencode's, per the
// README), and config.Validate deliberately accepts a clankerbar server under any key so long as it
// is handed CLANKERBAR_API_KEY.
func opencodeClankerbarTool(name string) string {
	const prefix = "clankerbar_"
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	return "mcp__clankerbar__" + strings.TrimPrefix(name, prefix)
}

// finish writes what the stream added up to onto res.
func (p *opencodeParse) finish(res *Result) {
	res.FinalMessage = p.lastText
	// What the session did with the backlog. Copied rather than accumulated in place because res does
	// not exist until the process has exited; see the `observed` field.
	res.Claim = p.observed.Claim
	res.Reports = p.observed.Reports
	res.ClankerbarCalls = p.observed.ClankerbarCalls
	res.UsageReported = p.sawUsage
	// The cost figure is its own question: a stream whose steps carried tokens but no cost reported no
	// money, and the durable record must say so rather than reporting 0.
	res.CostReported = p.sawCost
	if p.sawUsage {
		res.Tokens = p.total
		res.CostUSD = p.cost
		res.Usage = TokenUsage{
			Input:      p.in,
			Output:     p.out,
			Reasoning:  p.reason,
			CacheRead:  p.cRead,
			CacheWrite: p.cWrite,
		}
		res.Raw = map[string]any{
			"input_tokens": p.in, "output_tokens": p.out, "reasoning_tokens": p.reason,
			"cache_write_tokens": p.cWrite, "cache_read_tokens": p.cRead,
		}
	}
	// The finish reason is recorded even when the step carried no usage: a dead session's final step
	// can report zero tokens, and that reason is exactly the CLA-386 signal the driver needs to refuse
	// to hand the phase on.
	if p.lastReason != "" {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[FinishReasonKey] = p.lastReason
		// The CLA-398 quiet-death signature: a FINAL "unknown" step whose OWN usage was all-zero.
		if p.lastReason == FinishReasonUnknown && p.lastStepZeroUsage {
			res.Raw[TerminalReasonKey] = ZeroUsageReason
		}
		// CLA-584: the output-cap stall — a FINAL step that hit the output token limit with ZERO
		// output.
		if p.lastReason == FinishReasonLength && p.lastStepOut == 0 {
			res.Raw[TerminalReasonKey] = OutputCapReason
			res.Raw[OutputCapReasoningKey] = p.lastStepReason
		}
	}
	// The opencode session id, when the stream named one: the CLA-406 resurrection path resumes INTO
	// this session via `opencode run --session`, and needs the handle to survive onto the Result.
	if p.sessionID != "" {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[opencodeSessionIDKey] = p.sessionID
		res.SessionRef = p.sessionID
	}
	if p.errorType != "" {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[OpencodeErrorTypeKey] = p.errorType
	}
	// CLA-700: the events this session RECOVERED from, with no error event ending the stream.
	if p.recoveredErrors > 0 && !p.lastEventWasError {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[RecoveredErrorsKey] = p.recoveredErrors
	}
}

// opencodeBudgetRe recognizes a HARD budget/credit-exhaustion stop from the configured provider:
// out of credits, spend-cap or monthly/billing-limit reached, or an HTTP 402 Payment Required. The
// generic wordings that name an exhausted quota without saying which window it is — "usage limit
// reached", "insufficient quota" — live in opencodeGenericLimitRe, because the SAME words arrive
// on a rolling subscription window that a wait can outlast: a provider.quota event whose text is
// "Usage limit reached ...
var opencodeBudgetRe = regexp.MustCompile(`(?i)"status(code)?": ?402` +
	`|payment required` +
	`|out of (credit|credits|balance)` +
	`|insufficient (credit|credits|fund|funds|balance)` +
	`|(credit|token) balance (is )?(too low|depleted)` +
	`|spend[ -]?(cap|limit)|spending limit` +
	`|monthly (usage )?limit|billing (hard )?limit`)

// opencodeGenericLimitRe names an exhausted quota WITHOUT naming a window or a hard condition: the
// legacy "usage limit reached", and "insufficient [provider] quota".
var opencodeGenericLimitRe = regexp.MustCompile(`(?i)usage limit reached` +
	`|insufficient (?:[\w-]+ )?quota`)

// opencodeRollingQuotaRe recognizes the OTHER kind of provider limit: a rolling subscription window
// that a wait can outlast. The Command Code plan's 5-hour quota arrives as an HTTP 429 typed
// `provider.quota` and states the reset instant in its message; the loop supervises that wait and
// re-runs the session once the window lifts, so this must be reported as Limited with Stop UNSET
// rather than falling through to the transient ladder.
var opencodeRollingQuotaRe = regexp.MustCompile(`(?i)"type": ?"provider\.quota"` +
	`|\b\d+\s*[- ]?\s*(hour|hr|minute|min)s?\b[^.\n"]{0,40}\blimit\b` +
	`|\b(?:limit|quota)\b[^.\n"]{0,40}\b\d+\s*[- ]?\s*(hour|hr|minute|min)s?\b` +
	`|\b(session|daily|weekly)\s+usage\s+limit`)

// opencodeResetRe captures the reset instant a rolling quota STATES as RFC3339, e.g. "Your limit
// resets at 2026-10-20T15:56:48.251Z." When the message does not state one the parse returns the
// zero time and the supervised wait falls back to interval probing — a missing reset must not
// demote the limit to "no limit".
var opencodeResetRe = regexp.MustCompile(`(?i)resets?\s+at\s+` +
	`(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))`)

// opencodeResetInRe captures a RELATIVE reset a rolling quota states by duration, e.g. "Usage limit
// reached. Resets in 4 hours." or "reset in 45m".
var opencodeResetInRe = regexp.MustCompile(`(?i)resets?\s+in\s+(\d+)\s*(hours?|hrs?|minutes?|mins?|h|m)\b`)

func parseOpencodeReset(s string) time.Time { return parseOpencodeResetAt(s, time.Now()) }

// parseOpencodeResetAt is parseOpencodeReset with an injectable "now" for testing — the same
// split parseClaudeResetAt uses.
func parseOpencodeResetAt(s string, now time.Time) time.Time {
	if m := opencodeResetRe.FindStringSubmatch(s); m != nil {
		if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
			return t
		}
	}
	m := opencodeResetInRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return time.Time{}
	}
	switch strings.ToLower(m[2])[0] {
	case 'h':
		return now.Add(time.Duration(n) * time.Hour)
	case 'm':
		return now.Add(time.Duration(n) * time.Minute)
	}
	return time.Time{}
}

func (opencode) DetectLimit(res Result) Limit {
	// Scan only the harness's own diagnostics, NOT the agent's narration: with --format json the
	// assistant's text is a {"type":"text"} part on stdout, so a task that merely discusses billing
	// ("we're out of credits") must not trip a terminal stop.
	text := opencodeErrorText(res)
	// Hard budget stops keep winning: a monthly cap or an exhausted balance has no window to wait out,
	// however the event is typed.
	if opencodeBudgetRe.MatchString(text) {
		return Limit{Limited: true, Stop: true, Reason: "budget_exhausted"}
	}
	// CLA-752: the provider's own STRUCTURED statement comes before its wording.
	facts := opencodeErrorFactsOf(res)
	if window, planLimit := opencodeWindowLimit(facts, text); planLimit {
		return Limit{Limited: true, ResetAt: parseOpencodeReset(text), Reason: "usage_limit", Window: window}
	}
	if opencodeSubscriptionLimit(facts, text) {
		return Limit{Limited: true, ResetAt: parseOpencodeReset(text), Reason: "usage_limit"}
	}
	// A rolling subscription quota is a LIMIT the loop waits out, so Stop stays false.
	if opencodeRollingQuotaRe.MatchString(text) {
		return Limit{Limited: true, ResetAt: parseOpencodeReset(text), Reason: "usage_limit", Window: opencodeNamedWindow(text)}
	}
	// The generic exhausted-quota phrasings are a Stop only when nothing makes the limit waitable.
	if opencodeGenericLimitRe.MatchString(text) {
		if reset := parseOpencodeReset(text); !reset.IsZero() {
			return Limit{Limited: true, ResetAt: reset, Reason: "usage_limit"}
		}
		return Limit{Limited: true, Stop: true, Reason: "budget_exhausted"}
	}
	return Limit{}
}

// opencodeRateLimitWindows maps the provider's structured `rateLimit.window` token onto the label
// the loop reports. Keys are normalised (lower-case, separators stripped) because the provider's
// spelling is not stable: `FiveHour` and `five-hour` are the documented token too, and an
// exact-match map made those variants invisible, which is how a plan cap rode the transient ladder
// (review 1.review-1.4).
var opencodeRateLimitWindows = map[string]string{
	"fivehour": "5-hour",
	"weekly":   "weekly",
	"daily":    "daily",
}

// opencodeWindowToken normalises a structured window token before the closed map lookup: case- and
// separator-insensitive, so `fiveHour`, `five_hour` and `five-hour` are one window rather than one
// known token and two unknown ones.
func opencodeWindowToken(s string) string {
	return strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(strings.TrimSpace(s)))
}

// opencodeLimitWindowPhrase is the plan-limit wording a provider uses when it names no window in
// its body.
const opencodeLimitWindowPhrase = "usage limit for your plan"

// opencodeWindowLimit resolves the plan limit a failure states: the LABEL of the usage window it
// names (empty when it names none this adapter knows), and whether the failure is a plan limit at
// all. Its token is authoritative: when it names a window the closed map does not know, the window
// is UNKNOWN — the wording is not consulted over it, because a window the provider named is not
// something prose may relabel, and the plan's default is a guess the map's own contract forbids
// (reviews 1.review-1.4, 1.review-2.2).
func opencodeWindowLimit(f opencodeErrorFacts, text string) (window string, planLimit bool) {
	rateLimited := f.Status == 429 || strings.EqualFold(f.Code, "RATE_LIMITED")
	if !rateLimited {
		return "", false
	}
	if f.Window != "" {
		// The body NAMED a window. The closed map decides the label — including "no label" for a token
		// it does not know.
		return opencodeRateLimitWindows[opencodeWindowToken(f.Window)], true
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, opencodeLimitWindowPhrase) {
		return "", false
	}
	if window := opencodeNamedWindow(text); window != "" {
		return window, true
	}
	return "5-hour", true
}

// opencodeNamedWindow labels a window the text NAMES, when it names one. The window word only
// counts inside the limit STATEMENT — the same clause as a limit/quota word (either side of it,
// since the provider swaps the order).
var opencodeNamedWindowRe = regexp.MustCompile(`(?i)\b(weekly|daily|5[- ]hour)\b[^.\n"]{0,40}\b(?:usage\s+)?(?:limit|quota)\b` +
	`|\b(?:usage\s+)?(?:limit|quota)\b[^.\n"]{0,40}\b(weekly|daily|5[- ]hour)\b`)

func opencodeNamedWindow(text string) string {
	m := opencodeNamedWindowRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	token := m[1]
	if token == "" {
		token = m[2]
	}
	switch strings.ToLower(token) {
	case "weekly":
		return "weekly"
	case "daily":
		return "daily"
	}
	return "5-hour"
}

// opencodeErrorFacts is what the terminal error event states STRUCTURALLY: fields a caller can
// trust without reading prose.
type opencodeErrorFacts struct {
	// Status is the HTTP status the wrapper reported (error.status, or the older APIError shape's
	// error.data.statusCode).
	Status int
	// Code is the provider body's machine-readable error code, e.g. RATE_LIMITED.
	Code string
	// Window is the raw `rateLimit.window` token, when the body carried one.
	Window string
}

// opencodeErrorFactsOf reads the structured facts out of a finished Result's terminal error event.
// Neither is re-parsed by opencode, so `code` and `rateLimit` are only reachable by decoding that
// string here.
func opencodeErrorFactsOf(res Result) opencodeErrorFacts {
	line := opencodeTerminalErrorLine(res)
	if line == "" {
		return opencodeErrorFacts{}
	}
	var ev map[string]any
	if json.Unmarshal([]byte(line), &ev) != nil {
		return opencodeErrorFacts{}
	}
	errObj := opencodeJSONObject(ev["error"])
	dataObj := opencodeJSONObject(errObj["data"])

	f := opencodeMergeFacts(opencodeFactsFromObject(errObj), opencodeFactsFromObject(dataObj))
	for _, raw := range []any{
		opencodeJSONObject(errObj["response"])["body"],
		dataObj["responseBody"],
		ev["responseBody"],
		errObj["body"],
	} {
		body, ok := raw.(string)
		if !ok || body == "" {
			continue
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(body), &decoded) != nil {
			continue
		}
		f = opencodeMergeFacts(f, opencodeFactsFromObject(decoded))
		// The provider's body is normally an {error:{…}} envelope itself.
		f = opencodeMergeFacts(f, opencodeFactsFromObject(opencodeJSONObject(decoded["error"])))
	}
	return f
}

// opencodeFactsFromObject reads the three structured fields out of one decoded JSON object. Absent
// or wrongly-typed fields stay zero, and a `status` outside the HTTP range is not a status at all
// (review 1.review-1.5).
func opencodeFactsFromObject(o map[string]any) opencodeErrorFacts {
	var f opencodeErrorFacts
	if n, ok := opencodeJSONNumber(o["status"]); ok && n >= 100 && n <= 599 {
		f.Status = n
	} else if n, ok := opencodeJSONNumber(o["statusCode"]); ok && n >= 100 && n <= 599 {
		f.Status = n
	}
	if s, ok := o["code"].(string); ok {
		f.Code = s
	}
	if rl := opencodeJSONObject(o["rateLimit"]); rl != nil {
		if w, ok := rl["window"].(string); ok {
			f.Window = w
		}
	}
	return f
}

// opencodeMergeFacts keeps base's facts and fills only its holes from next.
func opencodeMergeFacts(base, next opencodeErrorFacts) opencodeErrorFacts {
	if base.Status == 0 {
		base.Status = next.Status
	}
	if base.Code == "" {
		base.Code = next.Code
	}
	if base.Window == "" {
		base.Window = next.Window
	}
	return base
}

// opencodeJSONObject is a decoded JSON object, or nil for anything else.
func opencodeJSONObject(v any) map[string]any {
	o, _ := v.(map[string]any)
	return o
}

// opencodeJSONNumber is a decoded JSON number as an int, or false.
func opencodeJSONNumber(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// opencodeErrorText collects the harness-level diagnostic text — all of stderr, plus a TERMINAL
// {"type":"error"} event on stdout — so a limit scan sees provider/transport errors but never the
// agent's own assistant text.
func opencodeErrorText(res Result) string {
	return res.scan(scanErrorText, func() string { return buildOpencodeErrorText(res) })
}

// buildOpencodeErrorText appends the stream's error event ONLY when it is how the session ENDED —
// the last typed event on stdout. stderr is scanned unconditionally: it is the harness's own
// diagnostic channel, written as the process winds down, and the fallback for the untyped failures
// the typed-event scan cannot see.
func buildOpencodeErrorText(res Result) string {
	var b strings.Builder
	b.WriteString(res.Stderr)
	// The error event is appended ONLY when the stream ENDED on it — the rule
	// opencodeTerminalErrorLine owns, shared with the structured fact scan so the two can never
	// disagree about which event is the failure.
	if line := opencodeTerminalErrorLine(res); line != "" {
		b.WriteByte('\n')
		b.WriteString(line)
	}
	return b.String()
}

// opencodeTerminalErrorLine returns the LAST typed event on the stream when it is a
// {"type":"error"} event, verbatim; "" otherwise.
func opencodeTerminalErrorLine(res Result) string {
	last := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type == "" {
			continue
		}
		last = line
	}
	if last == "" {
		return ""
	}
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(last), &ev) != nil || ev.Type != "error" {
		return ""
	}
	return last
}

// opencodeFatalRe: failures that are NEVER a blip, scanned before the transient patterns and before
// the heuristic below so neither can overrule them. A revoked key or a dead MCP server must end the
// run loudly at the first attempt: those are the failures an operator has to act on, and a daemon
// that retries them at the backoff ceiling instead reports "transient failure (exit 1) — retry
// 47" all night and never says what is actually wrong.
const opencodeFatalWords = `(authentication( \w+)? (failed|error|rejected)` +
	`|unauthorized|forbidden|permission denied` +
	`|invalid api key|api key (is )?(invalid|missing|not found|expired))`

// 2.x types its errors, and two of the types are fatal by definition, verified against 2.0.32:
// `provider.auth` (a 401, exit 1, no retry) and `provider.no-route` (an unknown provider or model,
// exit 1, no request sent).
var opencodeFatalRe = regexp.MustCompile(`(?i)"type": ?"provider\.(auth|no-route)"` +
	`|"status(code)?": ?40[13]` +
	`|"message": ?"[^"]*` + opencodeFatalWords +
	`|error: ?[^\n]*` + opencodeFatalWords)

// opencodeTransientRe: retryable server/network blips and API-level rate limits. The Console Go
// tool-schema rejection family (`unsupported_tool_schema` / `tool_count_limit`,
// anomalyco/opencode#43374 and #43378) is classified HERE, deliberately, rather than as a dedicated
// class with its own retry ladder: the gateway's enforcement is INCONSISTENT — #43374 records the
// same catalog accepted and refused seconds apart — so the identical retry often succeeds, which
// is exactly what the transient path does.
var opencodeTransientRe = regexp.MustCompile(`(?i)"type": ?"provider\.rate-limit"` +
	`|"status(code)?": ?(408|429|5\d\d)` +
	`|"isretryable": ?true` +
	`|overloaded|too many requests|rate ?limit` +
	`|unsupported_tool_schema|tool_count_limit` +
	`|"message": ?"transport` +
	`|transport (error|failure|reset|dropped)|transport (is )?clos(ed|ing)|transport-level` +
	`|stream (error|failed|closed|reset|ended|disconnected|interrupted)` +
	`|timed out waiting for response data|sse read timed out` +
	`|connection error|fetch failed|econnreset|econnrefused|etimedout|eai_again|socket hang up|network (error|timeout)`)

// No turn cap: Invocation.MaxTurns never reaches the CLI, so no exit can be attributed to one.
func (opencode) TurnCapped(Result) bool { return false }

// wallClockReason is the terminal_reason the ADAPTER writes into a Result when it ended the session
// for outliving Invocation.MaxSessionWallClock. Like tokenCeilingReason it is deliberately not
// something the CLI emits: the marker exists so the driver can tell an orderly cap from a genuine
// failure, and opencode's stream carries no reason field a task could write into anyway.
const wallClockReason = "wall_clock_capped"

func (opencode) WallClockCapped(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw["terminal_reason"].(string)
	return r == wallClockReason
}

// markWallClockCapped writes the adapter's own cap marker onto a Result, PRESERVING the parsed
// usage already there: unlike the claude token-ceiling kill, this session's spend was summed from
// step_finish events all the way to the kill, and replacing Raw would throw the per-step breakdown
// away while the budget still has to see what the session cost.
func (r *Result) markWallClockCapped() {
	if r.Raw == nil {
		r.Raw = map[string]any{}
	}
	r.Raw["terminal_reason"] = wallClockReason
}

// ZeroUsageUnknown reports whether this session ended with the CLA-398 quiet-death signature: a
// FINAL step_finish carrying reason "unknown" whose OWN usage was all-zero — whatever the earlier
// steps cost (a dead session may have done real paid work before the silent final step).
// The marker is the adapter's own (a terminal_reason it writes, never text the CLI or an agent
// could emit), and it exists so the driver can NAME the end instead of logging "iteration done
// (tokens=0 cost=$0.00)" over a session that produced nothing.
func (opencode) ZeroUsageUnknown(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw[TerminalReasonKey].(string)
	return r == ZeroUsageReason
}

// OutputCapNoOutput reports whether this session ended on the CLA-584 output-cap stall: a FINAL
// step_finish carrying reason "length" whose OWN output tokens were zero — the model spent the
// step's whole output budget thinking and emitted nothing.
func (opencode) OutputCapNoOutput(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw[TerminalReasonKey].(string)
	return r == OutputCapReason
}

// No mid-stream token ceiling: opencode sums per-step usage but the adapter never kills the
// process, so no exit of its can be attributed to one.
func (opencode) TokenCeilingHit(Result) bool { return false }

// TracksClaims is true as of CLA-365, and on 2.x only because the adapter takes every MCP server
// out of Code Mode (opencodeMCPOutOfCodeMode): opencode's `--format json` stream then carries each
// tool call's arguments AND its result on one terminal `tool_use` event, so the adapter reads the
// claim off the session the same way claude does and populates Result.Claim. HonoursMaxTurns stays
// false — Invocation.MaxTurns still never reaches the CLI.
func (opencode) Capabilities() Capabilities {
	// DeliversMessages is true because every non-probe session with a clankerbar MCP server runs
	// attached to a private `opencode serve` this adapter owns, and that server's session API steers
	// a message into the live run (opencode_inbox.go). A session with no such server has no delivery
	// target and simply receives nothing.
	return Capabilities{TracksClaims: true, ReportsCost: true, HonoursSessionWallClock: true, RunsTurnSequences: true, HonoursNoBacklogMCP: true, EmitsEvents: true, DeliversMessages: true, ResumesSessions: true}
}

func (o opencode) IsTransient(res Result) bool {
	text := opencodeErrorText(res)
	// A named stop wins over every retry path below, including the heuristic.
	if opencodeBudgetRe.MatchString(text) || opencodeRollingQuotaRe.MatchString(text) ||
		opencodeGenericLimitRe.MatchString(text) || opencodeFatalRe.MatchString(text) {
		return false
	}
	// CLA-752: the same veto for a plan cap stated STRUCTURALLY.
	if _, planLimit := opencodeWindowLimit(opencodeErrorFactsOf(res), text); planLimit {
		return false
	}
	// CLA-700: a session that RECOVERED from its provider errors is not retryable, however its exit
	// status reads.
	if RecoveredErrors(res) > 0 {
		return false
	}
	if opencodeTransientRe.MatchString(text) {
		return true
	}
	return o.IsUnclassifiedTransient(res)
}

// IsUnclassifiedTransient is the CLA-381 heuristic, and it is separate from the pattern scan
// because a caller has to be able to bound it — see harness.Adapter.IsUnclassifiedTransient for
// why that is a spending requirement and not a tidiness one. A usage event means the session
// authenticated and started doing paid work, so the failure came after the startup checks and is
// more likely a version-shaped stream drop this adapter has no pattern for than a config/auth
// refusal — that kind fails BEFORE any usage arrives, which is why the no-usage case stays a
// stop.
func (opencode) IsUnclassifiedTransient(res Result) bool {
	if res.ExitCode != 1 || !res.UsageReported {
		return false
	}
	text := opencodeErrorText(res)
	if opencodeBudgetRe.MatchString(text) || opencodeRollingQuotaRe.MatchString(text) ||
		opencodeGenericLimitRe.MatchString(text) || opencodeFatalRe.MatchString(text) {
		return false
	}
	// CLA-752: IsTransient's structural veto, for the same reason — the heuristic must not launder a
	// body-named plan cap into a bounded guess (review 1.review-1.2).
	if _, planLimit := opencodeWindowLimit(opencodeErrorFactsOf(res), text); planLimit {
		return false
	}
	// CLA-700: the same rule as IsTransient's — a session that recovered from its error events has
	// no unrecognised failure to guess at, so the heuristic must not launder its exit status into a
	// retry.
	if RecoveredErrors(res) > 0 {
		return false
	}
	return !opencodeTransientRe.MatchString(text)
}

// Diagnostic returns the scoped text IsTransient's PATTERN scans judged. See the claude adapter's
// Diagnostic for why the scope must match exactly.
func (opencode) Diagnostic(res Result) string { return opencodeErrorText(res) }

func (o opencode) Probe(ctx context.Context, in Invocation) (ProbeResult, error) {
	in.Probe = true
	res, err := o.Invoke(ctx, in)
	// Spend is taken off the Result on every path — see claude.Probe for why.
	out := ProbeResult{Tokens: res.Tokens, CostUSD: res.CostUSD}
	if err != nil {
		return out, err
	}
	return probeVerdict(out, res, o.DetectLimit)
}

func (opencode) ReadUsage(context.Context, Invocation) (Usage, error) {
	// opencode surfaces no remaining-quota/balance figure to a headless caller; billing lives with the
	// configured provider.
	return Usage{}, ErrUsageUnsupported
}
