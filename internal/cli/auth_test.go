package cli

// CLA-1038 at the command surface: `login` drives the device flow and saves the
// key owner-only, `logout` forgets it, `whoami` reports where the credential
// came from and proves it still works, and the existing commands fall back to the
// store when CLANKERBAR_API_KEY is unset.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/auth"
	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
)

const loginToken = "clk_login_test_secret_do_not_print"

// isolateStore points CLANKERBAR_AUTH_FILE at a fresh path and clears the env
// key, so a test starts logged out whatever the developer's machine holds.
func isolateStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "auth.json")
	t.Setenv(auth.EnvAuthFile, path)
	t.Setenv(auth.EnvAPIKey, "")
	return path
}

// loginPlane is the two device routes, scripted with one token response.
type loginPlane struct {
	srv       *httptest.Server
	tokenBody map[string]any
	tokenStat int

	mu      sync.Mutex
	code    map[string]any
	polls   []map[string]any
	reached int
}

func newLoginPlane(t *testing.T, tokenBody map[string]any) *loginPlane {
	t.Helper()
	p := &loginPlane{tokenBody: tokenBody, tokenStat: http.StatusOK}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)

		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/device/code":
			p.code = req
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":               "device-code-123",
				"user_code":                 "WDJB-MJHT",
				"verification_uri":          p.srv.URL + "/device",
				"verification_uri_complete": p.srv.URL + "/device?user_code=WDJB-MJHT",
				"expires_in":                600,
				"interval":                  1,
			})
		case "/api/device/token":
			p.polls = append(p.polls, req)
			p.reached++
			w.WriteHeader(p.tokenStat)
			_ = json.NewEncoder(w).Encode(p.tokenBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *loginPlane) request() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code
}

func (p *loginPlane) pollRequests() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.polls...)
}

func noWait(context.Context, time.Duration) error { return nil }

func TestLoginSavesUserCredential(t *testing.T) {
	path := isolateStore(t)
	p := newLoginPlane(t, map[string]any{
		"access_token": loginToken,
		"token_type":   "Bearer",
		"scope":        "user",
		"project_slug": nil,
	})

	var stdout, stderr bytes.Buffer
	err := loginRunWith(context.Background(), []string{"--host", p.srv.URL}, &stdout, &stderr, noWait)
	if err != nil {
		t.Fatalf("login: %v\n%s", err, stderr.String())
	}

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want login to keep it empty", stdout.String())
	}
	for _, want := range []string{"WDJB-MJHT", p.srv.URL + "/device?user_code=WDJB-MJHT", "Logged in to " + p.srv.URL, path} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr does not carry %q:\n%s", want, stderr.String())
		}
	}
	if strings.Contains(stdout.String(), loginToken) || strings.Contains(stderr.String(), loginToken) {
		t.Error("the minted key appears in the command's output")
	}

	code := p.request()
	if code["scope"] != auth.ScopeUser || code["client_name"] != "clankerbar-cli" {
		t.Errorf("device code request = %v, want scope=user client_name=clankerbar-cli", code)
	}
	polls := p.pollRequests()
	if len(polls) != 1 || polls[0]["device_code"] != "device-code-123" {
		t.Errorf("token polls = %v, want one poll with the grant's device_code", polls)
	}

	res, err := auth.Resolve(p.srv.URL)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Token != loginToken || res.Source != auth.SourceFile {
		t.Errorf("stored resolution = %s, want the minted token from the file", res)
	}
	if res.Scope != auth.ScopeUser {
		t.Errorf("stored scope = %q, want user", res.Scope)
	}
}

func TestLoginProjectScope(t *testing.T) {
	isolateStore(t)
	p := newLoginPlane(t, map[string]any{
		"access_token": loginToken,
		"token_type":   "Bearer",
		"scope":        "project",
		"project_slug": "acme",
	})

	var stdout, stderr bytes.Buffer
	if err := loginRunWith(context.Background(), []string{"--host", p.srv.URL, "--project-scope"}, &stdout, &stderr, noWait); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got := p.request()["scope"]; got != auth.ScopeProject {
		t.Errorf("device code scope = %v, want project", got)
	}
	res, err := auth.Resolve(p.srv.URL)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Scope != auth.ScopeProject || res.ProjectSlug != "acme" {
		t.Errorf("stored credential = %s, want project/acme", res)
	}
}

func TestLoginDefaultsToTheConfigOrigin(t *testing.T) {
	isolateStore(t)
	p := newLoginPlane(t, map[string]any{"access_token": loginToken, "scope": "user"})
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".config", "clankerbar")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"backlog_url":"`+p.srv.URL+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := loginRunWith(context.Background(), nil, &stdout, &stderr, noWait); err != nil {
		t.Fatalf("login with the config's origin: %v\n%s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want login to keep it empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Logged in to "+p.srv.URL) {
		t.Errorf("login did not use the config's backlog_url:\n%s", stderr.String())
	}
	res, err := auth.Resolve(p.srv.URL)
	if err != nil || res.Token != loginToken {
		t.Errorf("stored under the config origin? res=%s err=%v", res, err)
	}
}

func TestLoginRefusesWhenTheStoreIsDisabled(t *testing.T) {
	t.Setenv(auth.EnvAuthFile, os.DevNull)
	t.Setenv(auth.EnvAPIKey, "")
	p := newLoginPlane(t, map[string]any{"access_token": loginToken, "scope": "user"})

	var stdout, stderr bytes.Buffer
	err := loginRunWith(context.Background(), []string{"--host", p.srv.URL}, &stdout, &stderr, noWait)
	if !errors.Is(err, auth.ErrStoreDisabled) {
		t.Fatalf("login with the store disabled = %v, want ErrStoreDisabled", err)
	}
	// No browser round-trip for a key this process may not save.
	if p.request() != nil || len(p.pollRequests()) != 0 {
		t.Errorf("login started the device flow with the store disabled")
	}
}

func TestLoginDeniedSavesNothing(t *testing.T) {
	path := isolateStore(t)
	p := newLoginPlane(t, map[string]any{"error": "access_denied", "error_description": "The operator denied this request."})
	p.tokenStat = http.StatusBadRequest

	var stdout, stderr bytes.Buffer
	err := loginRunWith(context.Background(), []string{"--host", p.srv.URL}, &stdout, &stderr, noWait)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("login on a denial = %v, want the plane's denial", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Errorf("a denied login wrote the credential file: %v", statErr)
	}
}

func TestLogout(t *testing.T) {
	path := isolateStore(t)
	for _, origin := range []string{"https://one.example", "https://two.example"} {
		if err := auth.Save(origin, auth.Credential{Token: "token-" + origin, Scope: auth.ScopeUser}); err != nil {
			t.Fatalf("Save(%s): %v", origin, err)
		}
	}

	var stdout, stderr bytes.Buffer
	if err := logoutRun(context.Background(), []string{"--host", "https://one.example"}, &stdout, &stderr); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !strings.Contains(stderr.String(), "Logged out of https://one.example") {
		t.Errorf("logout = %q", stderr.String())
	}
	if _, _, ok, err := auth.Lookup("https://one.example"); err != nil || ok {
		t.Errorf("origin one is still stored: (%v, %v)", ok, err)
	}
	if _, _, ok, err := auth.Lookup("https://two.example"); err != nil || !ok {
		t.Errorf("logout removed the wrong origin: (%v, %v)", ok, err)
	}

	// Idempotent.
	stderr.Reset()
	if err := logoutRun(context.Background(), []string{"--host", "https://one.example"}, &stdout, &stderr); err != nil {
		t.Fatalf("second logout: %v", err)
	}
	if !strings.Contains(stderr.String(), "Not logged in") {
		t.Errorf("a second logout = %q, want a no-op line", stderr.String())
	}

	stderr.Reset()
	if err := logoutRun(context.Background(), []string{"--all"}, &stdout, &stderr); err != nil {
		t.Fatalf("logout --all: %v", err)
	}
	if !strings.Contains(stderr.String(), "1") {
		t.Errorf("logout --all = %q, want the dropped entry count", stderr.String())
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("logout --all left the store behind: %v", err)
	}
}

func TestLogoutAllAndHostAreExclusive(t *testing.T) {
	isolateStore(t)
	var stdout, stderr bytes.Buffer
	if err := logoutRun(context.Background(), []string{"--all", "--host", "https://clankerbar.com"}, &stdout, &stderr); err == nil {
		t.Fatal("logout accepted --all and --host together")
	}
}

// probeMCP is a minimal MCP server: it records every tools/call and answers with
// respond(tool).
func probeMCP(t *testing.T, respond func(tool string) (int, string)) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		calls = append(calls, req.Params.Name)
		mu.Unlock()
		status, body := respond(req.Params.Name)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const mcpOK = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`

func TestWhoamiReportsTheStoredCredential(t *testing.T) {
	path := isolateStore(t)
	srv, calls := probeMCP(t, func(string) (int, string) { return http.StatusOK, mcpOK })
	if err := auth.Save(srv.URL, auth.Credential{Token: loginToken, Scope: auth.ScopeUser}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := whoamiRun(context.Background(), []string{"--host", srv.URL}, &stdout, &stderr); err != nil {
		t.Fatalf("whoami: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"origin: " + srv.URL,
		"source: credential file " + path,
		"scope: user",
		"status: OK (list_projects succeeded)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("whoami output does not carry %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, loginToken) || strings.Contains(stderr.String(), loginToken) {
		t.Error("whoami printed the token")
	}
	if len(*calls) != 1 || (*calls)[0] != "list_projects" {
		t.Errorf("calls = %v, want one list_projects", *calls)
	}
}

func TestWhoamiReportsTheEnvSource(t *testing.T) {
	isolateStore(t)
	t.Setenv(auth.EnvAPIKey, "env-key")
	srv, _ := probeMCP(t, func(string) (int, string) { return http.StatusOK, mcpOK })

	var stdout, stderr bytes.Buffer
	if err := whoamiRun(context.Background(), []string{"--host", srv.URL}, &stdout, &stderr); err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if !strings.Contains(stdout.String(), "source: "+auth.EnvAPIKey) {
		t.Errorf("whoami output does not name the env source:\n%s", stdout.String())
	}
}

func TestWhoamiUnauthenticated(t *testing.T) {
	isolateStore(t)
	srv, calls := probeMCP(t, func(string) (int, string) { return http.StatusOK, mcpOK })

	var stdout, stderr bytes.Buffer
	err := whoamiRun(context.Background(), []string{"--host", srv.URL}, &stdout, &stderr)
	if err == nil {
		t.Fatal("whoami succeeded without a credential")
	}
	if !strings.Contains(err.Error(), "clankerbar login") {
		t.Errorf("the refusal does not name the fix: %v", err)
	}
	if !strings.Contains(stdout.String(), "source: none") {
		t.Errorf("whoami did not report the missing source:\n%s", stdout.String())
	}
	if len(*calls) != 0 {
		t.Errorf("whoami probed the plane without a credential: %v", *calls)
	}
}

func TestWhoamiReportsARejectedCredential(t *testing.T) {
	isolateStore(t)
	srv, _ := probeMCP(t, func(string) (int, string) { return http.StatusUnauthorized, `{"error":"unauthorized"}` })
	if err := auth.Save(srv.URL, auth.Credential{Token: loginToken, Scope: auth.ScopeUser}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := whoamiRun(context.Background(), []string{"--host", srv.URL}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("whoami on a rejected key = %v, want a rejection", err)
	}
}

// A project-scoped credential probes its project's endpoint, where the project
// tool set has no list_projects: the probe must fall back rather than report a
// healthy credential as broken.
func TestWhoamiProjectScopeUsesTheProjectEndpoint(t *testing.T) {
	isolateStore(t)
	srv, calls := probeMCP(t, func(tool string) (int, string) {
		if tool == "list_projects" {
			return http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"MCP error -32602: Tool list_projects not found"}],"isError":true}}`
		}
		return http.StatusOK, mcpOK
	})
	if err := auth.Save(srv.URL, auth.Credential{Token: loginToken, Scope: auth.ScopeProject, ProjectSlug: "acme"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := whoamiRun(context.Background(), []string{"--host", srv.URL}, &stdout, &stderr); err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if !strings.Contains(stdout.String(), "scope: project acme") {
		t.Errorf("whoami scope line:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "status: OK (get_backlog_summary succeeded)") {
		t.Errorf("whoami did not record the fallback tool:\n%s", stdout.String())
	}
	if len(*calls) != 2 || (*calls)[0] != "list_projects" || (*calls)[1] != "get_backlog_summary" {
		t.Errorf("calls = %v", *calls)
	}
}

func TestWhoamiRefusesAnUnsafeHost(t *testing.T) {
	isolateStore(t)
	var stdout, stderr bytes.Buffer
	err := whoamiRun(context.Background(), []string{"--host", "http://attacker.example"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("whoami accepted plain http to a non-loopback host")
	}
	if !strings.Contains(err.Error(), "--host") {
		t.Errorf("the refusal does not name the flag: %v", err)
	}
}

func TestAccountKeyForTheSupervisor(t *testing.T) {
	isolateStore(t)
	cfg := validCfg(t)
	const reason = "the roster is account-scoped"

	if _, err := accountKeyFor(cfg, "supervise", reason); err == nil || !strings.Contains(err.Error(), "clankerbar login") {
		t.Fatalf("accountKeyFor with no credential = %v, want a login hint", err)
	}

	if err := auth.Save("https://clankerbar.com", auth.Credential{Token: "project-key", Scope: auth.ScopeProject, ProjectSlug: "acme"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err := accountKeyFor(cfg, "supervise", reason)
	if err == nil || !strings.Contains(err.Error(), "project-scoped") || !strings.Contains(err.Error(), "--project-scope") {
		t.Fatalf("accountKeyFor with a project credential = %v, want the project-scope refusal", err)
	}

	if err := auth.Save("https://clankerbar.com", auth.Credential{Token: "user-key", Scope: auth.ScopeUser}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv(auth.EnvAPIKey, "")
	key, err := accountKeyFor(cfg, "supervise", reason)
	if err != nil {
		t.Fatalf("accountKeyFor with a user credential: %v", err)
	}
	if key != "user-key" {
		t.Errorf("accountKeyFor = %q, want the stored user key", key)
	}
	if got := os.Getenv(auth.EnvAPIKey); got != "user-key" {
		t.Errorf("%s after accountKeyFor = %q, want the exported key", auth.EnvAPIKey, got)
	}
}

// TestStoredCredentialFeedsUpload is the shared-fallback bar at a command: with
// CLANKERBAR_API_KEY unset, `upload` must still authenticate from the store.
func TestStoredCredentialFeedsUpload(t *testing.T) {
	isolateStore(t)
	const storedToken = "clk_upload_stored_token"
	plane := newFakePlane(t)
	if err := auth.Save(plane.srv.URL, auth.Credential{Token: storedToken, Scope: auth.ScopeUser}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	img := writeUploadFile(t, "shot.png", []byte("\x89PNG\r\n\x1a\n stored"))
	cfg := writeUploadConfig(t, plane.srv.URL+"/mcp/acme")

	var stdout, stderr bytes.Buffer
	if err := uploadRun(context.Background(), []string{"-c", cfg, img}, &stdout, &stderr); err != nil {
		t.Fatalf("upload with a stored credential: %v", err)
	}
	if got, want := stdout.String(), "asset:asset-1\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	_, calls, _ := plane.snapshot()
	if len(calls) != 1 || calls[0].auth != "Bearer "+storedToken {
		t.Errorf("MCP Authorization = %v, want the stored token", calls)
	}
	if strings.Contains(stdout.String(), storedToken) || strings.Contains(stderr.String(), storedToken) {
		t.Error("upload printed the stored token")
	}
}

func TestCredentialResolutionKeepsEnvPrecedence(t *testing.T) {
	isolateStore(t)
	const stored = "clk_stored"
	if err := auth.Save("https://clankerbar.com", auth.Credential{Token: stored, Scope: auth.ScopeUser}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg := validCfg(t)
	if got := cfg.CredentialOrigin(); got != "https://clankerbar.com" {
		t.Fatalf("fixture origin = %q", got)
	}

	t.Setenv(auth.EnvAPIKey, "env-key")
	res, err := credentialFor(cfg)
	if err != nil {
		t.Fatalf("credentialFor: %v", err)
	}
	if res.Token != "env-key" || res.Source != auth.SourceEnv {
		t.Errorf("credentialFor = %s, want the env key", res)
	}

	t.Setenv(auth.EnvAPIKey, "")
	res, err = credentialFor(cfg)
	if err != nil {
		t.Fatalf("credentialFor: %v", err)
	}
	if res.Token != stored || res.Source != auth.SourceFile {
		t.Errorf("credentialFor = %s, want the stored key", res)
	}
	if err := exportStoredCredential(res); err != nil {
		t.Fatalf("exportStoredCredential: %v", err)
	}
	if got := os.Getenv(auth.EnvAPIKey); got != stored {
		t.Errorf("%s after export = %q, want the stored key (children inherit it)", auth.EnvAPIKey, got)
	}
}

func TestDoctorFallsBackToTheStoredCredential(t *testing.T) {
	isolateStore(t)
	const stored = "clk_doctor_stored"
	if err := auth.Save("https://clankerbar.com", auth.Credential{Token: stored, Scope: auth.ScopeUser}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "clankerbar.json")
	if err := os.WriteFile(cfgPath, []byte(`{"harness":"claude","workdir":"`+dir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	e := okEnv()
	e.apiKey = ""
	var pollKey string
	e.newPoller = func(_, apiKey string) backlog.Poller {
		pollKey = apiKey
		return fakePoller{}
	}
	var out strings.Builder
	if err := doctorRun(context.Background(), &out, cfgPath, config.Overrides{}, e); err != nil {
		t.Fatalf("doctor with a stored credential failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "using the stored credential for https://clankerbar.com") {
		t.Errorf("doctor did not report the stored credential:\n%s", out.String())
	}
	if pollKey != stored {
		t.Errorf("doctor's poller key = %q, want the stored key", pollKey)
	}
	if strings.Contains(out.String(), stored) {
		t.Error("doctor printed the stored token")
	}
}
