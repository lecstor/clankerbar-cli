package cli

// doctor_setup.go — the pairing between a remedy and the clankerbar-setup skill.

import "fmt"

// setupSkillURL is the served remediation half of doctor: doctor says WHAT is wrong, and the
// numbered sections of this document say how the operator's agent fixes it. The section numbers
// below are the skill's, so a renumbering there makes these pointers stale — the skill is the
// source of truth.
const setupSkillURL = "clankerbar.com/skills/clankerbar-setup.md"

// setupSkillSection renders the pointer a setup-precondition remedy carries. The question-gated
// queue and stay-awake checks deliberately carry none: neither is an agent's to fix, and the
// skill says so.
func setupSkillSection(n int, topic string) string {
	return fmt.Sprintf("see clankerbar-setup section %d (%s) at %s", n, topic, setupSkillURL)
}
