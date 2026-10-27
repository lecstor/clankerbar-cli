package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- 9. permission policy ----------------------------------------------------

// opencodeEnvOverride finds a declaration of key that would override the
// adapter's own export of it — exec takes the last duplicate key, so ANY
// declared value wins — across all four env levels. On 2.x the key that matters
// is OPENCODE_CONFIG_CONTENT, which carries the fail-closed policy. It
// scans MOST-SPECIFIC-FIRST inside each project's scope (the
// project-per-harness block for this harness, then the project block), then the
// harness block, then top level: SessionEnv composes the child env per key by
// exactly that precedence, so when two levels declare the same name it is the
// more specific one the session actually carries. Naming a layer whose value
// loses the overlay would send the operator to edit the wrong line. Across
// projects the first declaring project is reported — each session carries only
// its own project's layers, so no single entry wins a whole multi-project run.
// The value reported is the declared source — the literal, or the command that
// produces it — never the resolved output.
func opencodeEnvOverride(cfg *config.Config, harnessName, key string) (where, value string, ok bool) {
	scan := func(label string, m config.EnvMap) (string, string, bool) {
		if v, found := m[key]; found {
			if v.IsCommand() {
				return label, "fromCommand: " + v.FromCommand, true
			}
			return label, v.Literal, true
		}
		return "", "", false
	}
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		if ph, exists := p.EnvPerHarness[harnessName]; exists {
			if w, v, found := scan(fmt.Sprintf("projects[%d].env_per_harness.%s", i, harnessName), ph); found {
				return w, v, true
			}
		}
		if w, v, found := scan(fmt.Sprintf("projects[%d].env", i), p.Env); found {
			return w, v, true
		}
	}
	if hc, exists := cfg.Harnesses[harnessName]; exists {
		if w, v, found := scan("harnesses."+harnessName+".env", hc.Env); found {
			return w, v, true
		}
	}
	if w, v, found := scan("env", cfg.Env); found {
		return w, v, true
	}
	return "", "", false
}

// checkPermissions reports on the run harness's permission policy. See
// checkPermissionsAll, which is what doctorChecks calls.
func checkPermissions(cfg *config.Config) check {
	return checkPermissionsNamed(cfg, "permissions", cfg.Harness)
}

// checkPermissionsAll reports on the policy of every harness this config will
// declares (CLA-366). The policy is per harness in both senses — a different
// mechanism each (a settings file, a sandbox flag, an env var) and a different
// file — so a mixed run that checked only `harness` would certify the policy of
// one session and say nothing about the other.
func checkPermissionsAll(cfg *config.Config) []check {
	names := cfg.PhaseHarnesses()
	out := make([]check, 0, len(names))
	for i, n := range names {
		label := "permissions"
		if i > 0 {
			label = "permissions[" + n + "]"
		}
		out = append(out, checkPermissionsNamed(cfg, label, n))
	}
	return out
}

func checkPermissionsNamed(cfg *config.Config, label, harnessName string) check {
	c := check{name: label}
	settingsPath := cfg.SessionFor(harnessName).SettingsPath
	switch harnessName {
	case "claude":
		if settingsPath == "" {
			c.status = warn
			c.detail = "no settings_path — the unattended session runs on the ambient allowlist"
			c.remedy = "set settings_path to a headless permission policy (its deny rules win over the config dir's)"
			return c
		}
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			c.status = fail
			c.detail = settingsPath + " is unreadable: " + err.Error()
			c.remedy = "create the settings file, or clear settings_path"
			return c
		}
		if !json.Valid(data) {
			// Claude rejects an unparseable --settings file at startup, so this kills
			// the first session rather than degrading it.
			c.status = fail
			c.detail = settingsPath + " is not valid JSON"
			c.remedy = "fix the JSON — the harness refuses to start with an unparseable --settings file"
			return c
		}
		c.status = pass
		c.detail = settingsPath + " parses"

	case "codex":
		// The adapter pins both axes itself, so there is nothing an operator can
		// leave incoherent today. Report what will actually be used.
		c.status = pass
		c.detail = "adapter pins --sandbox workspace-write --ask-for-approval never"

	case "opencode":
		return checkOpencodePermissions(cfg, label, harnessName)

	default:
		c.status = pass
		c.detail = "no permission-policy checks for " + harnessName
	}
	return c
}
