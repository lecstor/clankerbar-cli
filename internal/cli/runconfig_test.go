package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// rcPlaneEnv builds a doctorEnv whose run-config client serves one scripted
// answer for every project.
func rcPlaneEnv(answer func() (*plane.RunConfigState, error)) doctorEnv {
	e := defaultDoctorEnv()
	e.newRCfgAPI = func(mcpURL, apiKey string) plane.RunConfigAPI {
		return scriptedRC{answer: answer}
	}
	return e
}

type scriptedRC struct {
	answer func() (*plane.RunConfigState, error)
}

func (s scriptedRC) RunConfig(context.Context) (*plane.RunConfigState, error) {
	return s.answer()
}
func (s scriptedRC) ProposeRunConfig(context.Context, map[string]any, string) error {
	return errors.New("not used here")
}

func findRunConfigCheck(t *testing.T, checks []check) check {
	t.Helper()
	for _, c := range checks {
		if c.name == "run-config" || strings.HasPrefix(c.name, "run-config[") {
			return c
		}
	}
	t.Fatalf("no run-config check among %d checks", len(checks))
	return check{}
}

// Nothing stored: doctor says plainly that the local file rules are in force.
func TestCheckRunConfigs_NothingStoredReportsLocalRulesInForce(t *testing.T) {
	cfg := validCfg(t)
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return nil, plane.ErrNoConfig
	}))
	c := findRunConfigCheck(t, checks)
	if c.status != pass || !strings.Contains(c.detail, "local rules") {
		t.Errorf("status=%v detail=%q, want a PASS saying local rules are in force", c.status, c.detail)
	}
}

// A stored document in force is reported as such, with its version.
func TestCheckRunConfigs_StoredDocumentReportedAsInForce(t *testing.T) {
	cfg := validCfg(t)
	raw, _ := json.Marshal(map[string]any{"model": "plane-model"})
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return &plane.RunConfigState{Version: 3, Config: raw}, nil
	}))
	c := findRunConfigCheck(t, checks)
	if c.status != pass || !strings.Contains(c.detail, "stored v3") {
		t.Errorf("status=%v detail=%q, want a PASS naming stored v3", c.status, c.detail)
	}
}

// Machine-fit: a stored turn cap under opencode shape-validates on the plane
// but cannot fire here - that is exactly what this check exists to say.
func TestCheckRunConfigs_StoredTurnCapUnderOpencodeWarnedInert(t *testing.T) {
	cfg := validCfg(t)
	raw, _ := json.Marshal(map[string]any{"harness": "opencode", "max_turns": 40})
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return &plane.RunConfigState{Version: 2, Config: raw}, nil
	}))
	c := findRunConfigCheck(t, checks)
	if c.status != warn || !strings.Contains(c.detail, "INERT") || !strings.Contains(c.detail, "max_turns") {
		t.Errorf("status=%v detail=%q, want a WARN marking the stored max_turns INERT", c.status, c.detail)
	}
}

func TestCheckRunConfigs_UndecodableAndRefusedDocumentsWarnNotFail(t *testing.T) {
	cfg := validCfg(t)
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"undecodable", `{"model":`, "undecodable"},
		{"validate-refused", `{"harness":"not-a-harness"}`, "REFUSED locally"},
	} {
		raw := json.RawMessage(tc.raw)
		checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
			return &plane.RunConfigState{Version: 5, Config: raw}, nil
		}))
		c := findRunConfigCheck(t, checks)
		if c.status != warn || !strings.Contains(c.detail, tc.want) {
			t.Errorf("%s: status=%v detail=%q, want WARN containing %q", tc.name, c.status, c.detail, tc.want)
		}
	}
}

// Unwired: nothing to compare against, and the backlog wiring check already
// reports that gap - so this check stays silent rather than duplicating it.
func TestCheckRunConfigs_NotWiredIsSilent(t *testing.T) {
	cfg := validCfg(t)
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return nil, plane.ErrNotWired
	}))
	if c := findRunConfigCheckQuiet(checks); c != nil {
		t.Fatalf("unwired plane produced a run-config check: %+v", *c)
	}
}

func findRunConfigCheckQuiet(checks []check) *check {
	for i := range checks {
		if checks[i].name == "run-config" || strings.HasPrefix(checks[i].name, "run-config[") {
			return &checks[i]
		}
	}
	return nil
}

// Multi-project: each project's document is reported under its own label.
func TestCheckRunConfigs_MultiProjectGetsOneCheckPerProject(t *testing.T) {
	cfg := validCfg(t)
	cfg.Projects = []config.Project{{Slug: "alpha"}, {Slug: "beta"}}
	raw, _ := json.Marshal(map[string]any{})
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return &plane.RunConfigState{Version: 1, Config: raw}, nil
	}))
	for _, want := range []string{"run-config[alpha]", "run-config[beta]"} {
		var found bool
		for _, c := range checks {
			if c.name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no %q check among %d", want, len(checks))
		}
	}
}

// The propose-config command renders the movable half and records it PENDING;
// machine-local keys never ride along; an empty render is refused outright.
func TestProposeConfig_CommandFlow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"harness": "claude", "model": "opus", "models": {"strong": "opus"}, "workdir": "`+dir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLANKERBAR_API_KEY", "k-test")

	var gotDoc map[string]any
	var gotNotes string
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Params.Name != "propose_project_run_config" {
			t.Errorf("tool = %q", body.Params.Name)
		}
		called = true
		gotDoc, _ = body.Params.Arguments["config"].(map[string]any)
		gotNotes, _ = body.Params.Arguments["notes"].(string)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`))
	}))
	defer srv.Close()

	// Point the config's wiring at the fake plane so the command reaches it.
	if err := os.WriteFile(path, []byte(`{"harness": "claude", "model": "opus", "models": {"strong": "opus"}, "workdir": "`+dir+`", "backlog_url": "`+srv.URL+`/mcp/demo"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ProposeConfig(context.Background(), []string{"--config", path}); err != nil {
		t.Fatalf("ProposeConfig: %v", err)
	}
	if !called {
		t.Fatal("no proposal was recorded")
	}
	if gotDoc["harness"] != "claude" || gotDoc["model"] != "opus" {
		t.Errorf("document = %+v, want the file's movable dials", gotDoc)
	}
	if _, ok := gotDoc["workdir"]; ok {
		t.Error("the proposal carries workdir; machine paths never move to the plane")
	}
	if !strings.Contains(gotNotes, "propose-config") {
		t.Errorf("notes = %q, want the auto derivation note naming the command", gotNotes)
	}
}

// A config carrying nothing but the defaults imports exactly its one movable
// dial - and never invents entries for keys the file did not set.
func TestProposeConfig_MinimalConfigImportsExactlyItsMovableHalf(t *testing.T) {
	var gotDoc map[string]any
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Params.Name == "propose_project_run_config" {
			called = true
			gotDoc, _ = body.Params.Arguments["config"].(map[string]any)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"backlog_url": "`+srv.URL+`/mcp/demo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLANKERBAR_API_KEY", "k-test")

	if err := ProposeConfig(context.Background(), []string{"--config", path}); err != nil {
		t.Fatalf("ProposeConfig: %v", err)
	}
	if !called {
		t.Fatal("no proposal was recorded")
	}
	if len(gotDoc) != 1 || gotDoc["harness"] != "claude" {
		t.Errorf("document = %+v, want exactly the resolved harness dial", gotDoc)
	}
}
