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
	Tools       []string // tool names offered
	ToolResults []string // tool-result message contents, raw
	User        []string // user message contents, raw
}

// fakeOpenAI is a minimal OpenAI-compatible streaming chat server.
//
//   - status != 0: every chat request fails with that HTTP status.
//   - rounds set: a request that offers tools gets round k's tool calls, where
//     k is the number of assistant tool-call turns already in the conversation;
//     past the last round it answers "DONE". A request offering no tools (title
//     generation) gets a plain answer.
//   - otherwise: a plain "OK" answer with a usage block.
type fakeOpenAI struct {
	status int
	rounds [][]fakeCall
	delay  time.Duration

	hit  atomic.Int64
	mu   sync.Mutex
	reqs []fakeRequest
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
	var rec fakeRequest
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
	calls := func() [][]fakeCall {
		return [][]fakeCall{{
			{"write", map[string]any{"path": "w1.txt", "content": "in"}},
			{"write", map[string]any{"path": filepath.Join(outside, "w2.txt"), "content": "out"}},
			{"shell", map[string]any{"command": "echo b1 > b1.txt", "description": "in"}},
			{"shell", map[string]any{"command": "echo b2 > " + filepath.Join(outside, "b2.txt"), "description": "out"}},
			{"read", map[string]any{"path": "/etc/hosts"}},
			{"webfetch", map[string]any{"url": "http://127.0.0.1:1/"}},
		}}
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	t.Run("drain", func(t *testing.T) {
		fake := &fakeOpenAI{rounds: calls()}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		iso := isolate(t)
		work := t.TempDir()
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
		for _, denied := range []string{"webfetch", "websearch", "execute", "question", "skill", "subagent"} {
			if indexOfTool(offered, denied) >= 0 {
				t.Errorf("%s was offered; a tool-level deny must remove it", denied)
			}
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
				for _, gone := range []string{"webfetch", "websearch", "execute", "subagent", "question"} {
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
// observed off the stream, and the first-turn race is survived. Both config
// dialects: the adapter rewrites 1.x `mcp.<name>` into `mcp.servers.<name>`.
func TestOpencodeConformance_mcpClaim(t *testing.T) {
	requireOpencodeConformance(t)
	for _, dialect := range []string{"v1", "v2"} {
		t.Run(dialect, func(t *testing.T) {
			claim := []fakeCall{{"clankerbar_claim_task", map[string]any{"taskId": "T-9"}}}
			// Two claim rounds: the first may land before the MCP server has
			// connected; the model then calls again, as the warm-up note asks.
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
			if data, err := os.ReadFile(filepath.Join(mcpDir, "calls.jsonl")); err != nil || !strings.Contains(string(data), "claim_task") {
				t.Errorf("the fake MCP server saw no claim_task call (%v)", err)
			}
			sawNote := false
			for _, r := range fake.requests() {
				for _, u := range r.User {
					if strings.Contains(u, "connect in the background") {
						sawNote = true
					}
				}
			}
			if !sawNote {
				t.Error("the MCP warm-up note did not reach the model")
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
