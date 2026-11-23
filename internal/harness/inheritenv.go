package harness

// Filtering what a session inherits.

import "strings"

// inheritEnv returns env with only the variable NAMES in allow. Values are never inspected, logged
// or copied.
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
