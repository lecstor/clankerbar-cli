package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
)

// The pairing this pins: doctor says WHAT is wrong, and the served clankerbar-setup skill says how
// the operator's agent fixes it. A remedy for a setup precondition without the section it means is a
// finding with no instructions, which is the rediscovery loop the skill exists to end.

// The expectations here are literal on purpose: every other test derives its expected pointer from
// setupSkillSection, so without this one a typo in the URL or the format would move both sides of
// the comparison and ship a pointer the reader cannot follow.
func TestSetupSkillSectionNamesTheServedSkill(t *testing.T) {
	for _, tc := range []struct {
		section int
		topic   string
		want    string
	}{
		{
			1,
			"agent-instructions file",
			"see clankerbar-setup section 1 (agent-instructions file) at clankerbar.com/skills/clankerbar-setup.md",
		},
		{
			2,
			"pre-allow every toolchain",
			"see clankerbar-setup section 2 (pre-allow every toolchain) at clankerbar.com/skills/clankerbar-setup.md",
		},
	} {
		if got := setupSkillSection(tc.section, tc.topic); got != tc.want {
			t.Errorf("setupSkillSection(%d, %q) = %q, want %q", tc.section, tc.topic, got, tc.want)
		}
	}
}

// Section 1 is "The workdir needs an agent-instructions file", so the one workdir remedy that is
// about that file has to send the reader there — the multi-repo wording and the single-repo one.
func TestWorkdirRemedyNamesSetupSkillSectionOne(t *testing.T) {
	want := setupSkillSection(1, "agent-instructions file")
	dirs := map[string]string{
		"multi-repo parent": multiRepoParent(t, ""),
		"single repo":       t.TempDir(),
	}
	for name, dir := range dirs {
		t.Run(name, func(t *testing.T) {
			c := sessionCheck("workdir", dir, "/tmp/.mcp.json", "claude")
			if c.status != warn {
				t.Fatalf("workdir with no instructions file: got %v, want WARN (%s)", c.status, c.detail)
			}
			if !strings.Contains(c.remedy, want) {
				t.Errorf("remedy = %q, want it to name %q", c.remedy, want)
			}
		})
	}
}

// Section 2 is "A headless run fails closed, so pre-allow every toolchain first", so both verdicts
// that withhold a grant — absent and denied — point there.
func TestToolchainRemediesNameSetupSkillSectionTwo(t *testing.T) {
	want := setupSkillSection(2, "pre-allow every toolchain")
	t.Run("ungranted", func(t *testing.T) {
		c := checkToolchains(toolchainCfg(t, "go.mod", []string{"Bash(gh:*)"}, nil))
		if c.status != warn {
			t.Fatalf("ungranted toolchain: got %v, want WARN (%s)", c.status, c.detail)
		}
		if !strings.Contains(c.remedy, want) {
			t.Errorf("remedy = %q, want it to name %q", c.remedy, want)
		}
	})
	t.Run("denied", func(t *testing.T) {
		c := checkToolchains(toolchainCfg(t, "go.mod", []string{"Bash(go:*)"}, []string{"Bash(go:*)"}))
		if c.status != warn {
			t.Fatalf("denied toolchain: got %v, want WARN (%s)", c.status, c.detail)
		}
		if !strings.Contains(c.remedy, want) {
			t.Errorf("remedy = %q, want it to name %q", c.remedy, want)
		}
	})
}

// The two checks the setup skill names as having NO section, because neither is an agent's to fix:
// the queue gated behind the operator's unanswered questions, and whether the machine stays awake.
// Pinned as a negative so someone cannot "finish the set" by pointing them at a section that does
// not exist.
func TestNonAgentFixableRemediesCarryNoSetupSkillSection(t *testing.T) {
	gated := backlogCheck(context.Background(), "backlog", "https://example.test/api/backlog-summary",
		envWithPoll(backlog.Summary{Ready: 1, Claimable: 0, OpenQuestions: 2}, nil))
	if gated.remedy == "" {
		t.Fatal("question-gated queue: a WARN must carry a remedy line")
	}
	if strings.Contains(gated.remedy, "clankerbar-setup") {
		t.Errorf("question-gated queue is not an agent's to fix and has no setup-skill section, got %q", gated.remedy)
	}

	sleeping := checkPower(context.Background(), pmsetEnv("   PreventUserIdleSystemSleep       0\n", " sleep                10\n"))
	if sleeping.status != warn || sleeping.remedy == "" {
		t.Fatalf("idle-sleeping machine: got %v / %q, want a WARN with a remedy", sleeping.status, sleeping.remedy)
	}
	if strings.Contains(sleeping.remedy, "clankerbar-setup") {
		t.Errorf("stay-awake is not an agent's to fix and has no setup-skill section, got %q", sleeping.remedy)
	}

	unknown := okEnv()
	unknown.pmset = func(context.Context, ...string) (string, error) { return "", errors.New("exec: pmset not found") }
	if c := checkPower(context.Background(), unknown); strings.Contains(c.remedy, "clankerbar-setup") {
		t.Errorf("the unreadable-power remedy has no setup-skill section either, got %q", c.remedy)
	}
}
