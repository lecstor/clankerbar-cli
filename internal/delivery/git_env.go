package delivery

import "strings"

// gitNonInteractiveEnv is the interaction-hostile discipline every unattended
// git invocation shares: there is no terminal, and a git that decides to ask
// for a password would hang the driver or the supervisor rather than answer.
var gitNonInteractiveEnv = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ASKPASS=echo",
	"SSH_ASKPASS=echo",
	"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
	"GCM_INTERACTIVE=never",
}

// isOwnedGitEnv reports whether name is one of the non-interactive settings
// GitEnv always enforces itself.
func isOwnedGitEnv(name string) bool {
	switch name {
	case "GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_SSH_COMMAND", "GCM_INTERACTIVE":
		return true
	}
	return false
}

// IsGitConfigEnv reports whether name is a git env-config variable whose
// inherited value would collide with ScopeEnv's GIT_CONFIG_* entries.
// GIT_CONFIG_PARAMETERS is git's other command-line-config channel (`git -c`
// sets it for children); an inherited one can inject a competing insteadOf.
//
// Exported for the loop's git runner: it keeps its own operator-auth defaults,
// but the strip list has to be the one the scoping it appends was designed for.
func IsGitConfigEnv(name string) bool {
	return name == "GIT_CONFIG_COUNT" ||
		name == "GIT_CONFIG_PARAMETERS" ||
		strings.HasPrefix(name, "GIT_CONFIG_KEY_") ||
		strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// GitEnv builds the child environment for an unattended git invocation,
// stripping inherited GIT_CONFIG_* and owned non-interactive vars before
// appending ours and extraEnv: duplicate env keys have unspecified precedence
// across OS/libc, so an inherited GIT_CONFIG_COUNT could shadow the scoped
// credential rewrite in extraEnv.
//
// extraEnv entries naming an owned var are dropped rather than appended, so
// only this function decides those values.
func GitEnv(parent, extraEnv []string) []string {
	env := make([]string, 0, len(parent)+len(extraEnv)+len(gitNonInteractiveEnv))
	for _, e := range parent {
		name := envName(e)
		if isOwnedGitEnv(name) || IsGitConfigEnv(name) {
			continue
		}
		env = append(env, e)
	}
	env = append(env, gitNonInteractiveEnv...)
	for _, e := range extraEnv {
		if isOwnedGitEnv(envName(e)) {
			continue
		}
		env = append(env, e)
	}
	return env
}
