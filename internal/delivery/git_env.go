package delivery

import (
	"regexp"
	"strings"
)

// urlUserinfoRe matches URL userinfo (`scheme://user:pass@`) inside free-form
// git error output without crossing path/query/fragment delimiters or touching
// scp-style SSH addresses like `git@github.com:owner/repo.git`.
var urlUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^\s/?#'"<>]+)@`)

// redactURLCredentials scrubs embedded credentials from any URL appearing in s
// so failing scoped git commands never leak x-access-token URLs into logs or
// Check.Detail.
func redactURLCredentials(s string) string {
	return urlUserinfoRe.ReplaceAllString(s, "${1}redacted@")
}

// isOwnedGitEnv reports whether name is one of the non-interactive settings
// gitRunOut always enforces itself.
func isOwnedGitEnv(name string) bool {
	switch name {
	case "GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_SSH_COMMAND", "GCM_INTERACTIVE":
		return true
	}
	return false
}

// isGitConfigEnv reports whether name is a git env-config variable whose
// inherited value would collide with ScopeEnv's GIT_CONFIG_* entries.
func isGitConfigEnv(name string) bool {
	return name == "GIT_CONFIG_COUNT" ||
		strings.HasPrefix(name, "GIT_CONFIG_KEY_") ||
		strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// gitEnv builds the child environment for gitRunOut, stripping inherited
// GIT_CONFIG_* and owned non-interactive vars before appending ours and
// extraEnv so duplicate env keys never shadow scoped credentials.
func gitEnv(parent, extraEnv []string) []string {
	env := make([]string, 0, len(parent)+len(extraEnv)+5)
	for _, e := range parent {
		name := envName(e)
		if isOwnedGitEnv(name) || isGitConfigEnv(name) {
			continue
		}
		env = append(env, e)
	}
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"SSH_ASKPASS=echo",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
		"GCM_INTERACTIVE=never",
	)
	for _, e := range extraEnv {
		if isOwnedGitEnv(envName(e)) {
			continue
		}
		env = append(env, e)
	}
	return env
}
