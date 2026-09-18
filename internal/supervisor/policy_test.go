package supervisor

// The permission-policy gate's pure branches (CLA-549, phase 2c of
// docs/proposals/daemon-supervisor.md): checkPermissionPolicy verifies that
// the file every spawned harness's settings_path names exists, resolving the
// effective path exactly as SessionFor does (per-harness block wins for the
// run harness, else the top-level settings_path).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// writePolicy drops a permission policy file that Validate will accept.
func writePolicy(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "headless.json")
	if err := os.WriteFile(p, []byte(`{"permissions": {"allow": []}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The gate passes whatever the config names when the file exists, and refuses
// with ErrPolicyRefused when it does not. The check is on the EFFECTIVE path:
// a per-harness settings_path on the run harness wins over the top-level one,
// exactly as the child's SessionFor resolves it.
func TestCheckPermissionPolicy(t *testing.T) {
	policy := writePolicy(t)
	missing := filepath.Join(t.TempDir(), "headless-missing.json")
	dir := t.TempDir() // a directory is not a policy file

	cases := []struct {
		name string
		cfg  *config.Config
		want error // nil = passes
	}{
		{
			name: "no settings_path named",
			cfg:  &config.Config{Harness: "claude"},
			want: nil,
		},
		{
			name: "top-level settings_path exists",
			cfg:  &config.Config{Harness: "claude", SettingsPath: policy},
			want: nil,
		},
		{
			name: "top-level settings_path absent",
			cfg:  &config.Config{Harness: "claude", SettingsPath: missing},
			want: ErrPolicyRefused,
		},
		{
			name: "settings_path is a directory",
			cfg:  &config.Config{Harness: "claude", SettingsPath: dir},
			want: ErrPolicyRefused,
		},
		{
			name: "per-harness settings_path exists beats absent top-level",
			cfg: &config.Config{
				Harness:      "claude",
				SettingsPath: missing,
				Harnesses:    map[string]config.HarnessConfig{"claude": {SettingsPath: policy}},
			},
			want: nil,
		},
		{
			name: "absent per-harness settings_path beats present top-level",
			cfg: &config.Config{
				Harness:      "claude",
				SettingsPath: policy,
				Harnesses:    map[string]config.HarnessConfig{"claude": {SettingsPath: missing}},
			},
			want: ErrPolicyRefused,
		},
		{
			name: "non-run harness block is not checked without phases",
			cfg: &config.Config{
				Harness:   "claude",
				Harnesses: map[string]config.HarnessConfig{"opencode": {SettingsPath: missing}},
			},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPermissionPolicy(tc.cfg)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("checkPermissionPolicy = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("checkPermissionPolicy = %v, want %v", err, tc.want)
			}
			// The refusal names the path that was tried.
			named := tc.cfg.SettingsPath
			if hc := tc.cfg.Harnesses["claude"]; hc.SettingsPath != "" {
				named = hc.SettingsPath
			}
			if !strings.Contains(err.Error(), named) {
				t.Errorf("refusal %q does not name the policy path %q", err, named)
			}
		})
	}
}
