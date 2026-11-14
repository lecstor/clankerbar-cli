package harness

// Exec-binary conformance tests for the opencode adapter against opencode 2.x.
//
// These are the "how we call opencode" tests: they run the REAL opencode 2.x
// binary through the REAL adapter (`opencode{}.Invoke`), pointed at a hermetic
// fake OpenAI-compatible provider and a fake stdio MCP server (the test binary
// itself, see main_test.go). Zero spend: no request leaves the machine, and
// every test asserts the fake provider was actually hit.
//
// Opt-in, skipped (free, no spawn) otherwise:
//
//	CLANKERBAR_OPCODE_CONFORMANCE=1 go test ./internal/harness -run OpencodeConformance -v
//
// The binary is $CLANKERBAR_OPCODE_BIN, default ~/.opencode/bin/opencode (the
// official installer's location). TestMain puts its directory first on PATH so
// the adapter's plain `opencode` resolves to it; nothing here trusts whatever
// else PATH holds (an `opencode2` there may be an old pnpm beta). The build that
// ran is identified from the SESSION RECORD the run wrote (session_v2.version
// in the isolated data dir's opencode.db), never from PATH or `--version`.
//
// Isolation: HOME, XDG_CONFIG_HOME, XDG_DATA_HOME, XDG_CACHE_HOME and
// XDG_STATE_HOME point at fresh temp dirs and config_dir is set, so neither the
// operator's config, nor their auth database, nor the managed background
// service is touched; `--standalone` (the adapter always passes it) spawns a
// private server per run.
//
// The 429 case takes about 90 seconds: 2.0.32 retries a rate limit itself
// (~10 attempts over ~82s) before it exits.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	conformanceEnv    = "CLANKERBAR_OPCODE_CONFORMANCE"
	conformanceBinEnv = "CLANKERBAR_OPCODE_BIN"
	conformanceLive   = "CLANKERBAR_OPCODE_CONFORMANCE_LIVE"
	conformanceModel  = "OPENCODE_LIVE_MODEL" // e.g. opencode-go/deepseek-v4-flash
	conformancePrompt = "Reply with exactly: OK"
	// conformanceTimeout bounds each exec'd session. A plain context deadline,
	// never Invocation.MaxSessionWallClock, which would add a terminal marker.
	conformanceTimeout = 3 * time.Minute

	// The fake's usage block, per answered request, and the per-request cost it
	// comes to under fakeModelCost (3 $/M input, 15 $/M output).
	fakePromptTokens     = 18511
	fakeCompletionTokens = 2
	fakeRequestTokens    = fakePromptTokens + fakeCompletionTokens
	fakeRequestCost      = (fakePromptTokens*3 + fakeCompletionTokens*15) / 1e6
)

// conformanceBin is the opencode 2.x binary under test, or "" when it is not
// installed.
func conformanceBin() string {
	bin := os.Getenv(conformanceBinEnv)
	if bin == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		bin = filepath.Join(home, ".opencode", "bin", "opencode")
	}
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
		return ""
	}
	return bin
}

func requireOpencodeConformance(t *testing.T) string {
	t.Helper()
	if os.Getenv(conformanceEnv) == "" {
		t.Skipf("set %s=1 to exec the real opencode 2.x binary (see docs/harness-conformance.md)", conformanceEnv)
	}
	bin := conformanceBin()
	if bin == "" {
		t.Skipf("no opencode 2.x binary at $%s or ~/.opencode/bin/opencode", conformanceBinEnv)
	}
	// TestMain prepended its directory; prove the adapter's `opencode` is it.
	got, err := exec.LookPath("opencode")
	if err != nil || got != bin {
		t.Fatalf("PATH resolves opencode to %q (err %v), want %q - the adapter would exec a different build", got, err, bin)
	}
	return bin
}

// fakeCall is one tool call the fake provider asks for.
type fakeCall struct {
	Name string
	Args map[string]any
}

// fakeRequest is what one chat request carried, for assertions.
type fakeRequest struct {
	Model       string   // the model id the request asked for
	Tools       []string // tool names offered
	ToolResults []string // tool-result message contents, raw
	User        []string // user message contents, raw
}

// fakeOpenAI is a minimal OpenAI-compatible streaming chat server.
//
//   - status != 0: every chat request fails with that HTTP status.
//   - malformedTurns > 0: the first N requests that CARRY TOOLS stream the
//     anomalyco/opencode#43622 shape instead of an answer — content deltas with
//     no non-null finish_reason, then `[DONE]` — which 2.0.32 reads as
//     provider.invalid-output and retries itself. The retry is a fresh request,
//     so it gets the ordinary answer below.
//   - rounds set: a request that offers tools gets round k's tool calls, where
//     k is the number of assistant tool-call turns already in the conversation;
//     past the last round it answers "DONE". A request offering no tools (title
//     generation) gets a plain answer.
//   - otherwise: a plain "OK" answer with a usage block.
type fakeOpenAI struct {
	status         int
	rounds         [][]fakeCall
	delay          time.Duration
	malformedTurns int
	// stallChunks sends the opening role chunk and then NOTHING, holding the
	// response open until the client aborts. It is the silent-stream shape the
	// CLA-868 incident saw: no error, no bytes, no tokens.
	stallChunks bool

	hit       atomic.Int64
	toolTurns atomic.Int64
	mu        sync.Mutex
	reqs      []fakeRequest
}

func (f *fakeOpenAI) requests() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.reqs...)
}

func (f *fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"fake-model","object":"model","owned_by":"fake"}]}`)
		return
	case "/v1/chat/completions":
	default:
		http.NotFound(w, r)
		return
	}
	f.hit.Add(1)
	var body struct {
		Model string `json:"model"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []any           `json:"tool_calls"`
		} `json:"messages"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	rec := fakeRequest{Model: body.Model}
	turns := 0
	for _, t := range body.Tools {
		rec.Tools = append(rec.Tools, t.Function.Name)
	}
	for _, m := range body.Messages {
		switch {
		case m.Role == "tool":
			rec.ToolResults = append(rec.ToolResults, string(m.Content))
		case m.Role == "user":
			rec.User = append(rec.User, string(m.Content))
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			turns++
		}
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		if f.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(f.status)
		fmt.Fprintf(w, `{"error":{"message":"fake status %d","type":"fake_error","code":"fake_%d"}}`, f.status, f.status)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	chunk := func(delta map[string]any, finish any, usage any) {
		ch := map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "fake-model",
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if usage != nil {
			ch["usage"] = usage
		}
		b, _ := json.Marshal(ch)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	usage := map[string]any{"prompt_tokens": fakePromptTokens, "completion_tokens": fakeCompletionTokens, "total_tokens": fakeRequestTokens}
	chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)
	if f.stallChunks {
		// Headers and one chunk arrived; the stream now goes silent. The client
		// aborts its read when the adapter's chunkTimeout fires, which cancels
		// this request.
		<-r.Context().Done()
		return
	}
	if f.malformedTurns > 0 && len(rec.Tools) > 0 && int(f.toolTurns.Add(1)) <= f.malformedTurns {
		// The #43622 shape, byte-for-byte what drained on 2026-10-19: a complete
		// answer with every finish_reason null, then [DONE]. The AI SDK raises
		// "OpenAI Chat stream ended without finish_reason"; 2.x surfaces it as a
		// typed provider.invalid-output error event and retries the request
		// itself.
		chunk(map[string]any{"content": "O"}, nil, nil)
		chunk(map[string]any{"content": "K"}, nil, nil)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if len(f.rounds) > 0 && len(rec.Tools) > 0 && turns < len(f.rounds) {
		for i, c := range f.rounds[turns] {
			args, _ := json.Marshal(c.Args)
			chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": i, "id": fmt.Sprintf("call_%d", i), "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": string(args)},
			}}}, nil, nil)
		}
		chunk(map[string]any{}, "tool_calls", usage)
	} else {
		text := "OK"
		if len(f.rounds) > 0 && len(rec.Tools) > 0 {
			text = "DONE"
		}
		chunk(map[string]any{"content": text}, nil, nil)
		chunk(map[string]any{}, "stop", usage)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// fakeConfigDoc is the opencode 2.x-dialect config pointing at the fake
// provider, with a model `cost` block so cost accounting is exercised.
func fakeConfigDoc(providerURL string) map[string]any {
	return map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   map[string]any{"providerID": "fake", "model": "fake-model"},
		"provider": map[string]any{"fake": map[string]any{
			"npm":     "@ai-sdk/openai-compatible",
			"options": map[string]any{"baseURL": providerURL + "/v1", "apiKey": "test-key"},
			"models": map[string]any{"fake-model": map[string]any{
				"name": "Fake Model",
				"cost": map[string]any{"input": 3, "output": 15},
			}},
		}},
	}
}

func writeJSON(t *testing.T, doc map[string]any) string {
	t.Helper()
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeMCPEntry is a local MCP server entry that runs this test binary as the
// fake clankerbar server, logging calls into dir.
func fakeMCPEntry(t *testing.T, dir string) map[string]any {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"type":        "local",
		"command":     []string{self},
		"environment": map[string]any{fakeMCPDirEnv: dir},
	}
}

// conformanceIsolation is the env a conformance session runs under, plus the
// data dir the session record lands in.
type conformanceIsolation struct {
	env     []string
	dataDir string
	cfgDir  string
}

func isolate(t *testing.T) conformanceIsolation {
	t.Helper()
	iso := conformanceIsolation{dataDir: t.TempDir(), cfgDir: t.TempDir()}
	iso.env = []string{
		"HOME=" + t.TempDir(),
		"XDG_CONFIG_HOME=" + t.TempDir(),
		"XDG_DATA_HOME=" + iso.dataDir,
		"XDG_CACHE_HOME=" + t.TempDir(),
		"XDG_STATE_HOME=" + t.TempDir(),
	}
	return iso
}

func (iso conformanceIsolation) invocation(prompt, workdir, cfg string) Invocation {
	return Invocation{
		Prompt:        prompt,
		WorkDir:       workdir,
		MCPConfigPath: cfg,
		ConfigDir:     iso.cfgDir,
		Env:           iso.env,
	}
}

func invokeConformance(t *testing.T, in Invocation) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
	defer cancel()
	res, err := (opencode{}).Invoke(ctx, in)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return res
}

var versionLineRe = regexp.MustCompile(`version=([0-9][0-9A-Za-z._-]*)`)

// sessionRecordVersion reads the version of the build that created sid from the
// session record in the isolated data dir. sqlite3 reads session_v2.version;
// without sqlite3 the `cli starting ... version=` line the same run wrote into
// the isolated log is the fallback.
func sessionRecordVersion(t *testing.T, dataDir, sid string) string {
	t.Helper()
	db := filepath.Join(dataDir, "opencode", "opencode.db")
	if sq, err := exec.LookPath("sqlite3"); err == nil && sid != "" {
		out, err := exec.Command(sq, "-readonly", db, "select version from session_v2 where id='"+strings.ReplaceAll(sid, "'", "")+"'").Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			return strings.TrimSpace(string(out))
		}
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "opencode", "log", "opencode.log"))
	if err != nil {
		t.Fatalf("no session record: %s unreadable and %v", db, err)
	}
	m := versionLineRe.FindSubmatch(data)
	if m == nil {
		t.Fatal("no version in the session record")
	}
	return string(m[1])
}

// requireV2Record fails unless the session was created by a 2.x build.
func requireV2Record(t *testing.T, iso conformanceIsolation, res Result) {
	t.Helper()
	sid, _ := res.Raw[opencodeSessionIDKey].(string)
	ver := sessionRecordVersion(t, iso.dataDir, sid)
	t.Logf("session %s created by opencode %s (session record)", sid, ver)
	major, err := strconv.Atoi(strings.SplitN(ver, ".", 2)[0])
	if err != nil || major < 2 {
		t.Fatalf("session record version %q is not 2.x", ver)
	}
}

func requireHit(t *testing.T, f *fakeOpenAI) {
	t.Helper()
	if f.hit.Load() == 0 {
		t.Fatal("fake provider was never hit - the session resolved a different provider. NOTE: that run may have spent real tokens")
	}
}

// streamTypes lists the event types on a retained stdout, for a diagnostic.
func streamTypes(stdout string) []string {
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &ev) == nil && ev.Type != "" {
			out = append(out, ev.Type)
		}
	}
	return out
}

// toolEvents returns the tool_use parts on a Result's retained stdout.
func toolEvents(res Result) []opencodePart {
	var out []opencodePart
	for _, line := range strings.Split(res.Stdout, "\n") {
		var ev opencodeEvent
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &ev) == nil && ev.Type == "tool_use" && ev.Part != nil {
			out = append(out, *ev.Part)
		}
	}
	return out
}

// describeTools renders the tool_use events for a failure message.
func describeTools(res Result) string {
	var b strings.Builder
	for _, p := range toolEvents(res) {
		if p.State != nil {
			fmt.Fprintf(&b, "\n  %s %s %s %s", p.Tool, p.State.Status, p.State.Input, p.State.Error)
		}
	}
	return b.String()
}

func indexOfTool(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}
	return -1
}

// A plain answer: exit 0, the final message, and usage read from the session
// export. The stream carries NO usage for a plain answer on 2.x, so every
// figure here comes from the export, title generation included.
func TestOpencodeConformance_stop(t *testing.T) {
	requireOpencodeConformance(t)
	fake := &fakeOpenAI{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	iso := isolate(t)
	res := invokeConformance(t, iso.invocation(conformancePrompt, t.TempDir(), writeJSON(t, fakeConfigDoc(srv.URL))))
	requireHit(t, fake)
	requireV2Record(t, iso, res)

	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (stderr %.300s)", res.ExitCode, res.Stderr)
	}
	if res.FinalMessage != "OK" {
		t.Errorf("FinalMessage = %q, want OK", res.FinalMessage)
	}
	if res.Raw[opencodeUsageSourceKey] != opencodeUsageExport {
		t.Fatalf("usage_source = %v, want %q - the session export was not read", res.Raw[opencodeUsageSourceKey], opencodeUsageExport)
	}
	if !res.UsageReported {
		t.Error("UsageReported = false after a successful export")
	}
	hits := fake.hit.Load()
	if want := int(hits) * fakeRequestTokens; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d (%d requests x %d, title generation included)", res.Tokens, want, hits, fakeRequestTokens)
	}
	if want := float64(hits) * fakeRequestCost; res.CostUSD < want*0.999 || res.CostUSD > want*1.001 {
		t.Errorf("CostUSD = %v, want %v", res.CostUSD, want)
	}
	if got := res.Raw[FinishReasonKey]; got != "stop" {
		t.Errorf("finish_reason = %v, want stop (from the export's final assistant message)", got)
	}
	if (opencode{}).ZeroUsageUnknown(res) || (opencode{}).OutputCapNoOutput(res) {
		t.Error("a healthy session carries a terminal marker")
	}
}

// The adapter's policy, enforced by the real binary: tool-level denies remove
// the tools, external_directory refuses file tools outside the workdir, the
// working set is writable, and a read-only (probe) run loses edit and shell.
func TestOpencodeConformance_permissions(t *testing.T) {
	requireOpencodeConformance(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := func() [][]fakeCall {
		return [][]fakeCall{{
			{"write", map[string]any{"path": "w1.txt", "content": "in"}},
			{"write", map[string]any{"path": filepath.Join(outside, "w2.txt"), "content": "out"}},
			{"shell", map[string]any{"command": "echo b1 > b1.txt", "description": "in"}},
			{"shell", map[string]any{"command": "echo b2 > " + filepath.Join(outside, "b2.txt"), "description": "out"}},
			{"read", map[string]any{"path": "/etc/hosts"}},
			{"webfetch", map[string]any{"url": "http://127.0.0.1:1/"}},
			{"skill", map[string]any{"id": "probe"}},
			{"opencode_worktree_remove", map[string]any{"directory": victim}},
		}}
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	t.Run("drain", func(t *testing.T) {
		fake := &fakeOpenAI{rounds: calls()}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		iso := isolate(t)
		work := t.TempDir()
		// A real skill, so the skill call is refused by the policy, not by lookup.
		skillDir := filepath.Join(work, ".opencode", "skills", "probe")
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: probe\ndescription: probe\n---\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := invokeConformance(t, iso.invocation("work", work, writeJSON(t, fakeConfigDoc(srv.URL))))
		requireHit(t, fake)
		requireV2Record(t, iso, res)

		var offered []string
		for _, r := range fake.requests() {
			if len(r.Tools) > 0 {
				offered = r.Tools
				break
			}
		}
		t.Logf("tools offered under the drain policy: %v", offered)
		for _, denied := range []string{"webfetch", "websearch", "execute", "question", "subagent",
			"opencode_worktree_create", "opencode_worktree_list", "opencode_session_move", "opencode_session_rename", "opencode_models"} {
			if indexOfTool(offered, denied) >= 0 {
				t.Errorf("%s was offered; a tool-level deny must remove it", denied)
			}
		}
		// The MCP resource reads the policy keeps; a rename upstream goes red here.
		for _, kept := range []string{"opencode_list_mcp_resources", "opencode_read_mcp_resource"} {
			if indexOfTool(offered, kept) < 0 {
				t.Errorf("%s was not offered; the policy allows MCP resource reads", kept)
			}
		}
		// 2.2.x offers skill and opencode_worktree_remove whatever the policy says.
		// skill must be refused when called; worktree_remove must not delete a
		// directory opencode did not create.
		for _, tool := range []string{"skill", "opencode_worktree_remove"} {
			if indexOfTool(offered, tool) >= 0 {
				t.Logf("%s is offered under the drain policy (2.2.x does not filter it by permission)", tool)
			}
		}
		if !exists(filepath.Join(victim, "keep")) {
			t.Errorf("opencode_worktree_remove deleted a directory opencode did not create; events %s", describeTools(res))
		}
		skillRefused := false
		for _, p := range toolEvents(res) {
			if p.Tool == "skill" && p.State != nil && p.State.Status == "error" && strings.Contains(p.State.Error, "Permission denied: skill") {
				skillRefused = true
			}
		}
		if indexOfTool(offered, "skill") >= 0 && !skillRefused {
			t.Errorf("the skill call was not refused by the policy; events %s", describeTools(res))
		}
		for _, allowed := range []string{"read", "write", "edit", "shell", "glob", "grep"} {
			if indexOfTool(offered, allowed) < 0 {
				t.Errorf("%s was not offered; the drain policy allows it", allowed)
			}
		}
		if !exists(filepath.Join(work, "w1.txt")) {
			t.Errorf("write inside the workdir did not land; events %s", describeTools(res))
		}
		if exists(filepath.Join(outside, "w2.txt")) {
			t.Error("write OUTSIDE the workdir landed; external_directory must refuse it")
		}
		if !exists(filepath.Join(work, "b1.txt")) {
			t.Error("shell inside the workdir did not run")
		}
		denied := false
		for _, p := range toolEvents(res) {
			if p.Tool == "read" && p.State != nil && p.State.Status == "error" && strings.Contains(p.State.Error, "external_directory") {
				denied = true
			}
		}
		if !denied {
			t.Error("read of /etc/hosts was not refused by external_directory")
		}
		// The accepted gap (operator decision 2026-10-18, CLA-675): 2.x does not
		// path-gate the shell. Pinned as a log, not a failure, so a build that
		// starts gating it is noticed and the docs re-pinned.
		if exists(filepath.Join(outside, "b2.txt")) {
			t.Log("accepted gap reproduced: shell wrote outside the workdir (2.x does not path-gate shell commands)")
		} else {
			t.Log("NOTE: shell did NOT write outside the workdir - this build path-gates the shell; re-pin docs/opencode-build.md")
		}
	})

	t.Run("readonly", func(t *testing.T) {
		fake := &fakeOpenAI{rounds: calls()}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		iso := isolate(t)
		work := t.TempDir()
		in := iso.invocation("ignored", work, writeJSON(t, fakeConfigDoc(srv.URL)))
		in.Probe = true
		res := invokeConformance(t, in)
		requireHit(t, fake)
		requireV2Record(t, iso, res)
		for _, r := range fake.requests() {
			for _, gone := range []string{"edit", "write", "shell"} {
				if indexOfTool(r.Tools, gone) >= 0 {
					t.Errorf("%s offered to a read-only run", gone)
				}
			}
		}
		if exists(filepath.Join(work, "w1.txt")) || exists(filepath.Join(work, "b1.txt")) {
			t.Error("a read-only run wrote into the workdir")
		}
	})

	// A permission block in an ambient layer must not loosen the pinned policy.
	// The project opencode.json sits in the session's own checkout, so a
	// session can write it for the next spawn. Verified on 2.0.32: an
	// agent-level block overrides a top-level policy unless the agent is
	// pinned, and `default_agent` routes to an unpinned agent unless --agent is
	// passed. Each case must leave out-of-tree reads/writes refused, webfetch
	// unoffered, and in-tree writes working.
	for name, ambient := range map[string]map[string]any{
		"agent.build.permission": {"agent": map[string]any{"build": map[string]any{"permission": map[string]any{
			"external_directory": "allow", "read": "allow", "edit": "allow", "webfetch": "allow", "subagent": "allow"}}}},
		"agents.build.permissions": {"agents": map[string]any{"build": map[string]any{"permissions": []any{
			map[string]any{"action": "*", "resource": "*", "effect": "allow"}}}}},
		"default_agent": {"default_agent": "evil", "agent": map[string]any{"evil": map[string]any{
			"mode": "primary", "permission": map[string]any{"*": "allow", "external_directory": "allow"}}}},
		"top-level permission": {"permission": map[string]any{
			"read": "allow", "edit": "allow", "external_directory": "allow", "webfetch": "allow"}},
	} {
		t.Run("ambient "+name, func(t *testing.T) {
			fake := &fakeOpenAI{rounds: calls()}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			iso := isolate(t)
			work := t.TempDir()
			b, _ := json.Marshal(ambient)
			if err := os.WriteFile(filepath.Join(work, "opencode.json"), b, 0o644); err != nil {
				t.Fatal(err)
			}
			res := invokeConformance(t, iso.invocation("work", work, writeJSON(t, fakeConfigDoc(srv.URL))))
			requireHit(t, fake)
			for _, r := range fake.requests() {
				for _, gone := range []string{"webfetch", "websearch", "execute", "subagent", "question", "opencode_worktree_create"} {
					if indexOfTool(r.Tools, gone) >= 0 {
						t.Errorf("%s offered despite the pinned policy", gone)
					}
				}
			}
			if exists(filepath.Join(outside, "w2.txt")) {
				t.Errorf("out-of-tree write landed under an ambient %s block%s", name, describeTools(res))
				os.Remove(filepath.Join(outside, "w2.txt"))
			}
			if !exists(filepath.Join(work, "w1.txt")) {
				t.Errorf("in-tree write was locked out%s", describeTools(res))
			}
			os.Remove(filepath.Join(outside, "b2.txt"))
		})
	}

	// Last-match-wins on patterns, which the policy's ordering depends on: a
	// later, more specific deny beats an earlier allow. Exec'd raw, because the
	// adapter's own policy has no shell pattern to show it with.
	t.Run("pattern", func(t *testing.T) {
		fake := &fakeOpenAI{rounds: [][]fakeCall{{
			{"shell", map[string]any{"command": "git push origin main", "description": "push"}},
			{"shell", map[string]any{"command": "echo ok > p.txt", "description": "ok"}},
		}}}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		doc := fakeConfigDoc(srv.URL)
		doc["permission"] = map[string]any{"*": "deny", "shell": map[string]any{"*": "allow", "git push*": "deny"}}
		content, _ := json.Marshal(doc)
		iso := isolate(t)
		work := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "opencode", "run", "--standalone", "--format", "json", "--", "go")
		cmd.Dir = work
		// opencode takes its instance directory from $PWD, not the real cwd.
		cmd.Env = append(append(os.Environ(), iso.env...), "PWD="+work, "OPENCODE_CONFIG_CONTENT="+string(content))
		out, _ := cmd.Output()
		requireHit(t, fake)
		if !exists(filepath.Join(work, "p.txt")) {
			t.Errorf("the allowed shell command did not run; stream:\n%.3000s", out)
		}
		pushDenied := false
		for _, p := range toolEvents(Result{Stdout: string(out)}) {
			if p.Tool == "shell" && p.State != nil && strings.Contains(string(p.State.Input), "git push") && p.State.Status == "error" {
				pushDenied = true
			}
		}
		if !pushDenied {
			t.Errorf("git push was not denied by the later pattern; stream:\n%.2000s", out)
		}
	})
}

// MCP tools reach the session top-level (out of Code Mode), the claim is
// observed off the stream, and the first-turn race is GONE: the adapter's
// private-server preflight means the first tool-bearing request already offers
// clankerbar_claim_task (CLA-703). Both config dialects: the adapter rewrites
// 1.x `mcp.<name>` into `mcp.servers.<name>`.
func TestOpencodeConformance_mcpClaim(t *testing.T) {
	requireOpencodeConformance(t)
	for _, dialect := range []string{"v1", "v2"} {
		t.Run(dialect, func(t *testing.T) {
			claim := []fakeCall{{"clankerbar_claim_task", map[string]any{"taskId": "T-9"}}}
			// Two claim rounds are kept so a regression is VISIBLE rather than
			// fatal: if the preflight stops working, the first claim gets the
			// "No tool named" error and the model retries, and the assertions
			// below fail with the race named. On a healthy build the first
			// request already has the tools and the second round answers it.
			fake := &fakeOpenAI{rounds: [][]fakeCall{claim, claim}, delay: 300 * time.Millisecond}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			mcpDir := t.TempDir()
			doc := fakeConfigDoc(srv.URL)
			entry := fakeMCPEntry(t, mcpDir)
			if dialect == "v1" {
				entry["enabled"] = true
				doc["mcp"] = map[string]any{"clankerbar": entry}
			} else {
				doc["mcp"] = map[string]any{"servers": map[string]any{"clankerbar": entry}}
			}
			iso := isolate(t)
			res := invokeConformance(t, iso.invocation("work the backlog", t.TempDir(), writeJSON(t, doc)))
			requireHit(t, fake)
			requireV2Record(t, iso, res)

			events := toolEvents(res)
			var statuses []string
			for _, p := range events {
				if p.Tool == "execute" {
					t.Error("a tool_use for `execute` - the MCP server stayed in Code Mode")
				}
				if p.Tool == "clankerbar_claim_task" && p.State != nil {
					statuses = append(statuses, p.State.Status)
					if p.State.Status == "error" && strings.Contains(p.State.Error, "No tool named") {
						t.Log("first-turn MCP race reproduced: the first claim_task ran before the server connected")
					}
				}
			}
			if len(statuses) == 0 || statuses[len(statuses)-1] != "completed" {
				t.Fatalf("no completed top-level clankerbar_claim_task tool_use (statuses %v); stdout:\n%.3000s", statuses, res.Stdout)
			}
			if !res.Claim.Held() || res.Claim.TaskID != "T-9" || res.Claim.RunID != "run-1" {
				t.Errorf("Claim = %+v, want held T-9 / run-1", res.Claim)
			}
			// The successful call is also the fact the driver's no-MCP backstop
			// reads; it must be visible on the real stream, not only in a fixture.
			if res.ClankerbarCalls < 1 {
				t.Errorf("ClankerbarCalls = %d, want at least 1 for the completed claim_task", res.ClankerbarCalls)
			}
			if data, err := os.ReadFile(filepath.Join(mcpDir, "calls.jsonl")); err != nil || !strings.Contains(string(data), "claim_task") {
				t.Errorf("the fake MCP server saw no claim_task call (%v)", err)
			}
			// CLA-703: the race is removed, not described. The first request
			// that carries tools must already offer the clankerbar tool, and
			// the old warm-up note must be gone from every user message.
			sawToolOnFirstToolRequest := false
			for _, r := range fake.requests() {
				for _, u := range r.User {
					if strings.Contains(u, "connect in the background") {
						t.Error("the removed MCP warm-up note still reached the model")
					}
				}
				if len(r.Tools) == 0 || sawToolOnFirstToolRequest {
					continue
				}
				sawToolOnFirstToolRequest = true
				found := false
				for _, name := range r.Tools {
					if strings.Contains(name, "clankerbar_claim_task") {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("the first tool-bearing request did not offer clankerbar_claim_task (tools: %v)", r.Tools)
				}
			}
			if !sawToolOnFirstToolRequest {
				t.Error("no tool-bearing request reached the fake provider")
			}
			if want := int(fake.hit.Load()) * fakeRequestTokens; res.Tokens != want {
				t.Errorf("Tokens = %d, want %d", res.Tokens, want)
			}
		})
	}
}

// A claim_task result above 51,200 bytes is truncated by 2.x to a pointer at a
// saved file; the claim must still be observed, rehydrated from that file.
// Plane payloads reach this size (a get_task measured 56 KB).
func TestOpencodeConformance_largeClaim(t *testing.T) {
	requireOpencodeConformance(t)
	claim := []fakeCall{{"clankerbar_claim_task", map[string]any{"taskId": "T-9"}}}
	fake := &fakeOpenAI{rounds: [][]fakeCall{claim, claim}, delay: 300 * time.Millisecond}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	mcpDir := t.TempDir()
	entry := fakeMCPEntry(t, mcpDir)
	entry["environment"].(map[string]any)[fakeMCPPadEnv] = "120000"
	doc := fakeConfigDoc(srv.URL)
	doc["mcp"] = map[string]any{"servers": map[string]any{"clankerbar": entry}}
	iso := isolate(t)
	console := &strings.Builder{}
	in := iso.invocation("work the backlog", t.TempDir(), writeJSON(t, doc))
	in.Console = console
	res := invokeConformance(t, in)
	requireHit(t, fake)
	requireV2Record(t, iso, res)
	truncated := false
	for _, p := range toolEvents(res) {
		if p.Tool == "clankerbar_claim_task" && p.State != nil && p.State.Metadata != nil &&
			p.State.Metadata.Metadata != nil && p.State.Metadata.Metadata.Truncated {
			truncated = true
		}
	}
	if !truncated {
		t.Fatal("the claim result was not truncated - this build no longer exercises the envelope; re-pin the threshold")
	}
	if !res.Claim.Held() || res.Claim.TaskID != "T-9" || res.Claim.RunID != "run-1" {
		t.Errorf("Claim = %+v, want held T-9 / run-1 rehydrated from the saved output; console:\n%s", res.Claim, console)
	}
}

// Switching off a server the global config declares takes a FULL entry in the
// mcp config: verified on 2.0.32, a partial `{"enabled": false}` (1.x) or
// `servers.X: {"disabled": true}` (2.x) leaves it running, and doctor's
// opencode_mcp_disables check says so. The full forms must keep it off; the
// partial ones are logged so a build that changes this is noticed.
func TestOpencodeConformance_mcpDisable(t *testing.T) {
	requireOpencodeConformance(t)
	clone := func(e map[string]any, k string, v any) map[string]any {
		c := map[string]any{}
		for key, val := range e {
			c[key] = val
		}
		c[k] = v
		return c
	}
	for _, c := range []struct {
		name  string
		block func(e map[string]any) map[string]any
		full  bool
	}{
		{"full 1.x entry, enabled false", func(e map[string]any) map[string]any { return map[string]any{"other": clone(e, "enabled", false)} }, true},
		{"full 2.x entry, disabled true", func(e map[string]any) map[string]any {
			return map[string]any{"servers": map[string]any{"other": clone(e, "disabled", true)}}
		}, true},
		{"partial 1.x entry", func(map[string]any) map[string]any { return map[string]any{"other": map[string]any{"enabled": false}} }, false},
		{"partial 2.x entry", func(map[string]any) map[string]any {
			return map[string]any{"servers": map[string]any{"other": map[string]any{"disabled": true}}}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeOpenAI{delay: 1500 * time.Millisecond}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			iso := isolate(t)
			mcpDir := t.TempDir()
			entry := fakeMCPEntry(t, mcpDir)
			global, _ := json.Marshal(map[string]any{"mcp": map[string]any{"other": entry}})
			if err := os.WriteFile(filepath.Join(iso.cfgDir, "opencode.json"), global, 0o644); err != nil {
				t.Fatal(err)
			}
			doc := fakeConfigDoc(srv.URL)
			doc["mcp"] = c.block(entry)
			invokeConformance(t, iso.invocation(conformancePrompt, t.TempDir(), writeJSON(t, doc)))
			requireHit(t, fake)
			_, err := os.Stat(filepath.Join(mcpDir, "starts"))
			started := err == nil
			switch {
			case c.full && started:
				t.Error("a full entry with the off switch did not stop the server")
			case !c.full && started:
				t.Log("partial entry left the server running, as documented")
			case !c.full:
				t.Log("NOTE: a partial entry stopped the server on this build - re-pin docs/opencode-build.md and doctor's opencode_mcp_disables")
			}
		})
	}
}

// Resume with `-s <sid>` continues the SAME session headlessly, and the
// resumed run is charged only for its own requests, not the session's total.
func TestOpencodeConformance_resume(t *testing.T) {
	requireOpencodeConformance(t)
	fake := &fakeOpenAI{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	iso := isolate(t)
	in := iso.invocation(conformancePrompt, t.TempDir(), writeJSON(t, fakeConfigDoc(srv.URL)))
	first := invokeConformance(t, in)
	requireHit(t, fake)
	requireV2Record(t, iso, first)
	sid, _ := first.Raw[opencodeSessionIDKey].(string)
	if sid == "" {
		t.Fatal("no session id on the first run's stream")
	}
	before := fake.hit.Load()

	ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
	defer cancel()
	second, err := (opencode{}).runSession(ctx, in, opencodeResumeArgs(in, sid, "Reply with exactly: OK"))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, _ := second.Raw[opencodeSessionIDKey].(string); got != sid {
		t.Errorf("resumed run streamed session %q, want %q", got, sid)
	}
	if second.ExitCode != 0 || second.FinalMessage != "OK" {
		t.Errorf("resumed run: exit %d, final %q", second.ExitCode, second.FinalMessage)
	}
	if want := int(fake.hit.Load()-before) * fakeRequestTokens; second.Tokens != want {
		t.Errorf("resumed run Tokens = %d, want %d (its own requests only)", second.Tokens, want)
	}
	doc, err := (opencode{}).exportSession(ctx, in, sid)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range doc.Messages {
		if m.Type == "assistant" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("session holds %d assistant messages, want 2 (first run + resume)", n)
	}
}

// Two planned turns in ONE session, the second on a DIFFERENT model. This pins
// both halves of CLA-867 against the real binary:
//
//   - `-s <sid>` continues the same session, so the second turn reuses what the
//     first read;
//   - `-m <other model>` on the CONTINUED run really reaches the other model:
//     the fake provider records the model id each request named, the session
//     export records a `model-switched` message, and that turn is charged at
//     the second model's own rates.
//
// Prompt caching is per model, so the switched turn pays full input once; that
// is accepted (no cache reads are asserted). Verified on opencode 2.0.32 and
// 2.2.7.
func TestOpencodeConformance_turns(t *testing.T) {
	requireOpencodeConformance(t)
	fake := &fakeOpenAI{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	iso := isolate(t)
	doc := fakeConfigDoc(srv.URL)
	models := doc["provider"].(map[string]any)["fake"].(map[string]any)["models"].(map[string]any)
	models["fake-model-2"] = map[string]any{
		"name": "Fake Model Two",
		"cost": map[string]any{"input": 7, "output": 31},
	}
	in := iso.invocation("Reply with exactly: ONE", t.TempDir(), writeJSON(t, doc))
	in.Model = "fake/fake-model"
	// A runner phase is seeded with the live claim; the sequence refuses to run
	// a later turn under no lease, so the session needs one to continue.
	in.ResumeClaim = Claim{TaskID: "T-9", Ref: "CLA-867", RunID: "run-1"}
	in.Turns = []Turn{{Prompt: "Reply with exactly: TWO", Model: "fake/fake-model-2"}}
	res := invokeConformance(t, in)
	requireHit(t, fake)
	requireV2Record(t, iso, res)

	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stderr %.300s)", res.ExitCode, res.Stderr)
	}
	if want := []string{"OK", "OK"}; !reflect.DeepEqual(res.TurnMessages, want) {
		t.Errorf("TurnMessages = %q, want %q (one message per turn)", res.TurnMessages, want)
	}
	if res.FinalMessage != "OK" {
		t.Errorf("FinalMessage = %q, want the final turn's OK", res.FinalMessage)
	}
	if res.TurnStopped != 0 || res.TurnStopReason != "" {
		t.Errorf("TurnStopped = %d (%q), want 0: both turns ran", res.TurnStopped, res.TurnStopReason)
	}

	// The switched turn reached the OTHER model. The request that carries the
	// second prompt is the continued run's.
	reqs := fake.requests()
	var secondTurn *fakeRequest
	for i := range reqs {
		for _, u := range reqs[i].User {
			if strings.Contains(u, "TWO") {
				secondTurn = &reqs[i]
			}
		}
	}
	if secondTurn == nil {
		t.Fatalf("no request carried the second turn's prompt; requests: %+v", reqs)
	}
	if secondTurn.Model != "fake-model-2" {
		t.Errorf("the continued turn's request named model %q, want fake-model-2 - -m was not honoured on the resumed session", secondTurn.Model)
	}
	// The first turn ran on the invocation's model, so the switch is a change
	// rather than a single model throughout.
	sawFirstModel := false
	for _, r := range reqs {
		if r.Model == "fake-model" {
			sawFirstModel = true
		}
	}
	if !sawFirstModel {
		t.Errorf("no request named the first turn's model; requests: %+v", reqs)
	}

	// Usage sums across both turns: title generation + first answer + second
	// answer are three requests.
	if want := 3 * fakeRequestTokens; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d summed across both turns", res.Tokens, want)
	}
	fakeSecondTurnCost := (fakePromptTokens*7 + fakeCompletionTokens*31) / 1e6
	if want := 2*fakeRequestCost + fakeSecondTurnCost; res.CostUSD < want*0.999 || res.CostUSD > want*1.001 {
		t.Errorf("CostUSD = %v, want %v (the switched turn at the second model's rates)", res.CostUSD, want)
	}

	// The session itself holds both turns, and records the model switch: the
	// resumed turn was not a second session.
	sid, _ := res.Raw[opencodeSessionIDKey].(string)
	if sid == "" {
		t.Fatal("no session id on the result")
	}
	ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
	defer cancel()
	exported, err := (opencode{}).exportSession(ctx, in, sid)
	if err != nil {
		t.Fatal(err)
	}
	assistants, switched := 0, false
	for _, m := range exported.Messages {
		switch m.Type {
		case "assistant":
			assistants++
		case "model-switched":
			switched = true
		}
	}
	if assistants != 2 {
		t.Errorf("session holds %d assistant messages, want 2 (both turns)", assistants)
	}
	if !switched {
		t.Errorf("the session records no model-switched message; the export shows the switch was not a model change")
	}
}

// Typed errors and how the adapter classifies them.
func TestOpencodeConformance_errors(t *testing.T) {
	requireOpencodeConformance(t)
	cases := []struct {
		name      string
		status    int
		model     string
		wantType  string
		transient bool
	}{
		{"auth", http.StatusUnauthorized, "", "provider.auth", false},
		{"noroute", 0, "fake/no-such-model", "provider.no-route", false},
		{"ratelimit", http.StatusTooManyRequests, "", "provider.rate-limit", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeOpenAI{status: c.status}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			iso := isolate(t)
			in := iso.invocation(conformancePrompt, t.TempDir(), writeJSON(t, fakeConfigDoc(srv.URL)))
			in.Model = c.model
			res := invokeConformance(t, in)
			if c.name != "noroute" {
				requireHit(t, fake)
			} else if fake.hit.Load() != 0 {
				t.Error("no-route sent a request")
			}
			if res.ExitCode != 1 {
				t.Errorf("ExitCode = %d, want 1", res.ExitCode)
			}
			if got := res.Raw[OpencodeErrorTypeKey]; got != c.wantType {
				t.Errorf("error type = %v, want %s", got, c.wantType)
			}
			if got := (opencode{}).IsTransient(res); got != c.transient {
				t.Errorf("IsTransient = %v, want %v; diagnostic %.300s", got, c.transient, (opencode{}).Diagnostic(res))
			}
			if (opencode{}).DetectLimit(res).Limited {
				t.Error("DetectLimit fired on a non-budget error")
			}
			if res.UsageReported {
				t.Errorf("UsageReported = true for a run the provider never answered (tokens %d)", res.Tokens)
			}
		})
	}
}

// TestOpencodeConformance_recoveredProviderError is the CLA-700 probe, run
// against the REAL binary: what does opencode 2.x do with a chat stream that
// drains with every finish_reason null (#43622)?
//
// The recorded answer, and the reason the driver may not judge a phase by its
// exit status:
//
//   - the session RETRIES the request itself and finishes the work — the stream
//     carries the typed provider.invalid-output error event and then the retry's
//     own steps and final answer, which is what makes this a RECOVERED error
//     rather than a failed session;
//   - and it exits NON-ZERO anyway (1 on 2.0.32). That exit status is
//     indistinguishable from a genuine failure at the process level, which is
//     exactly how two delivered phases were stranded on 2026-10-19.
//
// 2.2.x reports the same recovery as a `retry` event carrying the error and
// exits 0. Both shapes are accepted, and both must be recorded as recovered, so
// a later non-zero exit still reads as a recovery.
func TestOpencodeConformance_recoveredProviderError(t *testing.T) {
	requireOpencodeConformance(t)
	fake := &fakeOpenAI{malformedTurns: 1}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	iso := isolate(t)
	res := invokeConformance(t, iso.invocation(conformancePrompt, t.TempDir(), writeJSON(t, fakeConfigDoc(srv.URL))))
	requireHit(t, fake)
	requireV2Record(t, iso, res)

	// The retry really happened: the fake served the malformed shape once, and a
	// later tool-bearing request got the ordinary answer.
	if got := fake.toolTurns.Load(); got < 2 {
		t.Fatalf("the session sent %d tool-bearing request(s), want at least 2 — opencode did not retry the malformed stream, so this fixture is not exercising the recovery", got)
	}

	// The exit status: 1 on 2.0.32 (error event), 0 on 2.2.x (retry event).
	if res.ExitCode != 0 && res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 0 or 1 after a recovered provider error (stderr %.300s)", res.ExitCode, res.Stderr)
	}

	// The event sequence: the typed error (an error or retry event), and typed
	// events AFTER it.
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	errAt, errKind := -1, ""
	for i, line := range lines {
		var ev opencodeEvent
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type == "" {
			continue
		}
		if ev.Type == "error" || ev.Type == "retry" {
			errAt, errKind = i, ev.Type
			if ev.Error == nil || ev.Error.Type != "provider.invalid-output" {
				t.Errorf("line %d: %s event error = %+v, want type provider.invalid-output (the #43622 shape)", i, ev.Type, ev.Error)
			}
		}
	}
	if errAt < 0 {
		t.Fatalf("the stream carries no error or retry event; the fixture did not reproduce #43622:\n%s", res.Stdout)
	}
	if errAt == len(lines)-1 {
		t.Fatalf("the %s event is the LAST event on the stream — the session did not recover, so this fixture is not #43622:\n%s", errKind, res.Stdout)
	}
	if RecoveredErrors(res) == 0 {
		t.Errorf("exit %d with a %s event followed by %d event(s), but the adapter recorded no recovered errors", res.ExitCode, errKind, len(lines)-errAt-1)
	}
	if res.FinalMessage != "OK" {
		t.Errorf("FinalMessage = %q, want OK — the retried turn's answer is the session's final message", res.FinalMessage)
	}
	// And the classification the driver actually reads: a recovered session is
	// neither transient nor a failure, whatever the exit status says.
	if (opencode{}).IsTransient(res) {
		t.Errorf("IsTransient = true for a recovered session (exit %d); diagnostic = %.300q", res.ExitCode, (opencode{}).Diagnostic(res))
	}
	if (opencode{}).DetectLimit(res).Limited {
		t.Error("DetectLimit fired on a recovered provider.invalid-output event")
	}
	t.Logf("exit=%d %s-event recovered=%d final=%q usage_reported=%v tokens=%d", res.ExitCode, errKind, RecoveredErrors(res), res.FinalMessage, res.UsageReported, res.Tokens)
}

// CLA-877: a provider stream that goes silent must ERROR at the adapter's
// chunkTimeout, not hang until the phase idle watchdog kills the session. Pins
// the whole chain against the real binary: the adapter's materialised config
// carries `providers.<id>.settings.chunkTimeout` (2.0.32 accepts the pin), and
// opencode really aborts the stalled SSE read. The production bound is five
// minutes; the test shrinks it so the proof costs seconds.
func TestOpencodeConformance_chunkTimeout(t *testing.T) {
	requireOpencodeConformance(t)
	old := opencodeProviderChunkTimeout
	opencodeProviderChunkTimeout = 1500 * time.Millisecond
	t.Cleanup(func() { opencodeProviderChunkTimeout = old })

	fake := &fakeOpenAI{stallChunks: true}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	iso := isolate(t)
	in := iso.invocation(conformancePrompt, t.TempDir(), writeJSON(t, fakeConfigDoc(srv.URL)))
	// Naming the model route is what lets the adapter name the provider whose
	// stream it pins; the fake config resolves the same model object.
	in.Model = "fake/fake-model"
	start := time.Now()
	res := invokeConformance(t, in)
	elapsed := time.Since(start)
	requireHit(t, fake)
	if elapsed > 30*time.Second {
		t.Fatalf("the stalled session took %s — the adapter's chunkTimeout did not end it (context bound %s)",
			elapsed, conformanceTimeout)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode = 0 for a stream that never finished; the chunk timeout must error, not look like a clean end (final %.200q)", res.FinalMessage)
	}
	diag := strings.ToLower((opencode{}).Diagnostic(res))
	if !strings.Contains(diag, "timed out") && !strings.Contains(diag, "timeout") {
		t.Errorf("the diagnostic does not name the stream timeout: %.600q", diag)
	}
	t.Logf("stalled stream ended in %s: provider hits=%d transport events=%d exit=%d error_type=%v diagnostic=%.300q",
		elapsed, fake.hit.Load(), strings.Count(res.Stdout, "provider.transport"), res.ExitCode, res.Raw[OpencodeErrorTypeKey], diag)
	t.Logf("STREAM TYPES: %s", strings.Join(streamTypes(res.Stdout), " "))
}

// The content layer is merged after OPENCODE_CONFIG. The adapter's permission
// policy and MCP rewrite live in the content layer, so they hold only while that
// ordering does. Exec'd raw: through the adapter both layers carry the same
// provider, which would mask the ordering.
func TestOpencodeConformance_contentPin(t *testing.T) {
	requireOpencodeConformance(t)
	fake := &fakeOpenAI{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	dead := fakeConfigDoc("http://127.0.0.1:1")
	dead["model"] = map[string]any{"providerID": "dead", "model": "fake-model"}
	dead["provider"] = map[string]any{"dead": dead["provider"].(map[string]any)["fake"]}
	content, _ := json.Marshal(fakeConfigDoc(srv.URL))
	iso := isolate(t)
	ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "opencode", "run", "--standalone", "--format", "json", "--", conformancePrompt)
	cmd.Dir = t.TempDir()
	cmd.Env = append(append(os.Environ(), iso.env...), "PWD="+cmd.Dir,
		"OPENCODE_CONFIG="+writeJSON(t, dead), "OPENCODE_CONFIG_CONTENT="+string(content))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	requireHit(t, fake)
	if !strings.Contains(string(out), `"text":"OK"`) {
		t.Errorf("final answer not on the stream: %.300s", out)
	}
}

// TestOpencodeConformance_live is the paid, non-hermetic gate: ONE real turn
// against whatever provider this machine has configured (named via
// OPENCODE_LIVE_MODEL). Opt-in via CLANKERBAR_OPCODE_CONFORMANCE_LIVE=1, and it
// uses the operator's real config and auth.
func TestOpencodeConformance_live(t *testing.T) {
	if os.Getenv(conformanceLive) == "" {
		t.Skipf("set %s=1 (and %s) for the live one-turn gate", conformanceLive, conformanceModel)
	}
	model := os.Getenv(conformanceModel)
	if model == "" {
		t.Fatalf("%s must name a model opencode can resolve, e.g. opencode-go/deepseek-v4-flash", conformanceModel)
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("no opencode binary on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
	defer cancel()
	res, err := (opencode{}).Invoke(ctx, Invocation{Prompt: conformancePrompt, Model: model, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
	}
	if !res.UsageReported {
		t.Error("UsageReported = false - expected the session export to report usage")
	}
	t.Logf("live turn: tokens=%d cost=$%.4f final=%q source=%v", res.Tokens, res.CostUSD, res.FinalMessage, res.Raw[opencodeUsageSourceKey])
}
