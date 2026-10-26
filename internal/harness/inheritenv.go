package harness

// Filtering what a session inherits (CLA-714).
//
// Every adapter used to hand exec a copy of os.Environ() unchanged (plus the
// config's `env` overlay). So a config `env` block was ADDITIVE, never a
// whitelist: an unattended, model-driven session could read every variable in
// the shell that started the supervisor, including credentials it has no use
// for. Invocation.EnvAllowlist is the config-declared answer to "what may this
// session see", and inheritEnv is the ONE place the inherited half is filtered.
//
// The config's `env` entries are appended AFTER this filter and are therefore
// always present: they are declarations for the session, not inherited state,
// and that is how GH_TOKEN and CLAUDE_CODE_OAUTH_TOKEN arrive when the ambient
// copy is dropped. A fromCommand that produces one of those values runs in the
// daemon, before this filter, so it may read anything the daemon can.

import "strings"

// inheritEnv returns env with only the variable NAMES in allow. A nil or empty
// allow means "inherit everything" and returns env itself, byte-identical to
// the pre-allowlist behaviour — the default no config changes.
//
// The comparison is by exact name: an allowlist naming HOME does not admit
// HOME_DIRECTORY, and it does not admit an entry with no '=' at all (nothing
// can read a name with no value). Values are never inspected, logged or
// copied.
func inheritEnv(env []string, allow []string) []string {
	if len(allow) == 0 {
		return env
	}
	keep := make(map[string]struct{}, len(allow))
	for _, name := range allow {
		keep[name] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		if _, ok := keep[kv[:i]]; ok {
			out = append(out, kv)
		}
	}
	return out
}
