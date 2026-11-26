package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// opencodePermission is the fail-closed policy the adapter writes into the `permission` key of the
// config it pins via OPENCODE_CONFIG_CONTENT (see opencodeSessionConfig): a path-scoped
// PermissionConfig, not a flat tool map. Everything else fails closed via the `*` catch-all, and
// the network/exfil tools (webfetch/websearch) are denied in both shapes.
func opencodePermission(readOnly bool, workdir string, extraDirs []string) string {
	b, _ := json.Marshal(opencodePermissionPolicy(readOnly, workdir, extraDirs))
	return string(b)
}

// opencodePermissionPolicy is opencodePermission before marshalling, so the session config can
// embed it as an object rather than a string.
func opencodePermissionPolicy(readOnly bool, workdir string, extraDirs []string) map[string]any {
	if readOnly {
		// Probes check provider liveness or session coherence only: deny every tool
		// (filesystem, shell, network, MCP) so a probe cannot inspect the repo or
		// mutate the backlog.
		return map[string]any{
			"*":         "deny",
			"edit":      "deny",
			"shell":     "deny",
			"execute":   "deny",
			"webfetch":  "deny",
			"websearch": "deny",
		}
	}
	_, abs := opencodeWorkdirPatterns(workdir)
	read := map[string]string{"**": "allow", "../**": "deny", opencodeMCPResourcePattern: "allow"}
	edit := map[string]string{"**": "allow", "../**": "deny"}
	ext := map[string]string{"*": "deny", abs: "allow"}
	scopeExtraDirs(read, edit, ext, opencodeInstanceDir(workdir), extraDirs)
	perm := map[string]any{
		"read":               read,
		"edit":               edit,
		"external_directory": ext,
		// MCP tools must survive the `*` catch-all — see the function doc.
		"*_*": "allow",
		// The read-only search tools: grep/glob/list. Their asks carry the search pattern (a regex, a
		// glob, or the resolved directory), never a workdir-relative path, so a path-scoped rule could
		// not match them and the catch-all would leave the tools denied — the CLA-390 bug.
		"grep": map[string]string{"**": "allow"},
		"glob": map[string]string{"**": "allow"},
		"list": map[string]string{"**": "allow"},
		// The exfil guards, explicit in both shapes.
		"webfetch":  "deny",
		"websearch": "deny",
	}
	// Code Mode, denied by name: see the function doc.
	perm["execute"] = "deny"
	perm["opencode_*"] = "deny"
	perm["opencode_list_mcp_resources"] = "allow"
	perm["opencode_read_mcp_resource"] = "allow"
	perm["shell"] = "allow"
	perm["*"] = "deny"
	return perm
}

// scopeExtraDirs widens the policy's three path-scoped rules to cover extraDirs: the absolute
// external_directory allow, and the instance-relative read/edit form. Emitting it would either
// overwrite the "../**" deny with an allow or sort after it and beat it ("../../**"), turning the
// policy's only stated escape boundary into an allow of the whole filesystem.
func scopeExtraDirs(read, edit, ext map[string]string, base string, extraDirs []string) {
	for _, dir := range extraDirs {
		if dir == "" {
			continue
		}
		_, abs := opencodeWorkdirPatterns(dir)
		ext[abs] = "allow"
		rel, err := filepath.Rel(base, absOrSelf(dir))
		if err != nil || rel == "." || isDotDotChain(rel) || !strings.HasPrefix(filepath.ToSlash(rel), "../") {
			continue
		}
		relGlob := filepath.ToSlash(rel)
		read[relGlob+"/**"] = "allow"
		edit[relGlob+"/**"] = "allow"
		read[relGlob] = "allow"
		edit[relGlob] = "allow"
	}
}

// isDotDotChain reports whether a slash-separated relative path is nothing but ".." components -
// what filepath.Rel returns for a directory that CONTAINS the base.
func isDotDotChain(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg != ".." {
			return false
		}
	}
	return true
}

// opencodeInstanceDir is the directory a session's asks are relative to: the workdir, or the
// process cwd when the workdir is empty (where the session would actually run).
func opencodeInstanceDir(workdir string) string {
	if workdir == "" {
		if cwd, err := os.Getwd(); err == nil {
			return cwd
		}
	}
	return absOrSelf(workdir)
}

func absOrSelf(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// opencodeWorkdirPatterns derives two pattern forms from a directory. rootRel is the root-relative
// form 1.x read/edit asks used (the absolute path minus its leading separator); 2.x never asks in
// it, and the policy no longer emits it, but the helper keeps returning it for the tests that pin
// its derivation.
func opencodeWorkdirPatterns(workdir string) (rootRel, abs string) {
	if workdir == "" {
		var err error
		if workdir, err = os.Getwd(); err != nil {
			// Unresolvable cwd: match NOTHING rather than everything — the policy's stated posture is
			// fail-closed.
			return "/**", "\x00/**"
		}
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return "/**", "\x00/**"
	}
	abs = filepath.Clean(abs)
	vol := filepath.VolumeName(abs)
	rel := strings.TrimPrefix(abs, vol)
	rel = strings.TrimLeft(rel, `/\`)
	return filepath.ToSlash(rel) + "/**", filepath.ToSlash(abs) + "/**"
}

// withoutAmbientOpencodeConfig drops an inherited OPENCODE_CONFIG: the driver decides which file a
// session is handed, and an ambient value would hand one over even when config resolved none.
func withoutAmbientOpencodeConfig(env []string) []string {
	return withoutEnvKey(env, "OPENCODE_CONFIG")
}

// withoutEnvKey drops every assignment of one variable.
func withoutEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (opencode) env(in Invocation) ([]string, error) {
	// The session's INSTANCE directory.
	env := pinPWD(inheritEnv(withoutAmbientOpencodeConfig(os.Environ()), in.EnvAllowlist), in.WorkDir)
	if in.ConfigDir != "" {
		env = append(env, "OPENCODE_CONFIG_DIR="+in.ConfigDir)
	}
	if in.MCPConfigPath != "" {
		env = append(env, "OPENCODE_CONFIG="+in.MCPConfigPath)
	}
	content, err := opencodeSessionConfigWithEnv(in, env)
	if err != nil {
		// FAIL CLOSED. The alternative - spawn without the content layer - is a session with no
		// permission policy at all on 2.x, claiming from whichever project the ambient config names.
		return nil, err
	}
	env = append(env, "OPENCODE_CONFIG_CONTENT="+string(content))
	env = append(env, in.Env...)
	return env, nil
}
