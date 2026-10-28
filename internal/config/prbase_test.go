package config

import (
	"errors"
	"testing"
)

// ResolvePRBase is the ONE rule for a repo's merge target, shared by the phase
// briefs and the deterministic close-out: the operator's pr_base override, else
// the integration branch when origin has it, else origin's default branch, else
// the integration branch. A close-out resolving it from config alone refused PRs
// the briefs had correctly told sessions to open.
func TestResolvePRBase(t *testing.T) {
	prbaseErr := errors.New("git ls-remote: no such remote")
	tests := []struct {
		name        string
		cfg         *Config
		repo        string
		probe       PRBaseProbe
		want        string
		wantSource  PRBaseSource
		wantDefault string
		wantErr     bool
	}{
		{
			name:       "the per-repo override wins without a probe",
			cfg:        &Config{PRBase: map[string]string{"lecstor/makespdf": "integration"}},
			repo:       "lecstor/makespdf",
			probe:      nil, // must not even be needed
			want:       "integration",
			wantSource: PRBaseOverride,
		},
		{
			name:       "no override and no probe: the configured integration branch",
			cfg:        &Config{IntegrationBranch: "release"},
			repo:       "acme/widgets",
			want:       "release",
			wantSource: PRBaseIntegrationFallback,
		},
		{
			name:       "no override and no probe: the default integration branch",
			cfg:        &Config{},
			repo:       "acme/widgets",
			want:       DefaultIntegrationBranch,
			wantSource: PRBaseIntegrationFallback,
		},
		{
			name: "the integration branch present on origin wins over its default",
			cfg:  &Config{},
			repo: "acme/widgets",
			probe: func(integration string) (bool, string, error) {
				if integration != DefaultIntegrationBranch {
					t.Errorf("probe asked about %q, want %q", integration, DefaultIntegrationBranch)
				}
				return true, "main", nil
			},
			want:        DefaultIntegrationBranch,
			wantSource:  PRBaseIntegrationOnOrigin,
			wantDefault: "main",
		},
		{
			name: "no integration branch on origin: the remote's default branch",
			cfg:  &Config{},
			repo: "acme/widgets",
			probe: func(string) (bool, string, error) {
				return false, "integration", nil
			},
			want:        "integration",
			wantSource:  PRBaseOriginDefault,
			wantDefault: "integration",
		},
		{
			name: "a probe that cannot answer falls back to the integration branch",
			cfg:  &Config{},
			repo: "acme/widgets",
			probe: func(string) (bool, string, error) {
				return false, "", prbaseErr
			},
			want:       DefaultIntegrationBranch,
			wantSource: PRBaseIntegrationFallback,
			wantErr:    true,
		},
		{
			name: "a detached remote HEAD leaves the integration branch in place",
			cfg:  &Config{},
			repo: "acme/widgets",
			probe: func(string) (bool, string, error) {
				return false, "", nil
			},
			want:       DefaultIntegrationBranch,
			wantSource: PRBaseIntegrationFallback,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolvePRBase(tc.cfg, "acme", tc.repo, tc.probe)
			if got.Base != tc.want {
				t.Errorf("Base = %q, want %q", got.Base, tc.want)
			}
			if got.Source != tc.wantSource {
				t.Errorf("Source = %v, want %v", got.Source, tc.wantSource)
			}
			if got.Default != tc.wantDefault {
				t.Errorf("Default = %q, want %q", got.Default, tc.wantDefault)
			}
			if gotErr := got.Err != nil; gotErr != tc.wantErr {
				t.Errorf("Err = %v, want error=%v", got.Err, tc.wantErr)
			}
		})
	}
}

// The project's own integration_branch beats the top-level field, exactly as the
// raw accessor resolves it — the resolver must not flatten that precedence.
func TestResolvePRBaseProjectIntegrationBranch(t *testing.T) {
	cfg := &Config{
		IntegrationBranch: "top",
		Projects:          []Project{{Slug: "acme", IntegrationBranch: "project"}},
	}
	got := ResolvePRBase(cfg, "acme", "acme/widgets", nil)
	if got.Base != "project" {
		t.Errorf("Base = %q, want the project's integration branch %q", got.Base, "project")
	}
}
