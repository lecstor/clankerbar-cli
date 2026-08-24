package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// This file makes session env DAEMON-OWNED (CLA-462). Before it, load-bearing
// session environment could only be declared at one level and resolved once at
// Validate, so an operator who launched the binary without the cb-run wrapper
// spawned sessions with no GH_TOKEN — and every such session died later, at
// `git push` or `gh`, with an error that named neither the cause nor the fix.
// The root cause was ownership: the LAUNCHER decided whether sessions could
// push. Restart mechanisms cannot fix that class of failure, because re-exec
// preserves whatever environment was broken.
//
// The shape that replaces it: `env` maps at FOUR levels — top level, per
// harness, per project, and per project-per-harness — overlaid most-specific-
// wins in the same order as the existing MCP wiring (ResolveMCPConfig), where a
// value is either a literal string (including the "@path" file-reference form)
// or {"fromCommand": "..."} run FRESH AT EVERY SPAWN so tokens rotate without a
// restart. A failing command REFUSES the spawn rather than degrading: a session
// without its declared env is the incident, not a mode to run in.

// EnvCommandTimeout bounds one fromCommand execution. Short on purpose: these
// commands are token prints (`gh auth token`), not work, and a hung one would
// otherwise hang every session spawn behind it.
const EnvCommandTimeout = 10 * time.Second

// envCommandTimeout is the value the default runner actually applies. It is a
// variable only so a test can shrink it and prove the deadline fires; nothing
// outside this package should read it.
var envCommandTimeout = EnvCommandTimeout

// RunEnvCommand executes one declared fromCommand and returns its trimmed
// stdout. Package-level so tests can stub it (a counter script proves
// freshness; a failing stub proves the refusal); the default runs the command
// under `sh -c` with the command timeout. It comes from the operator's own
// config file — the same trust root as everything else in the config — never
// from a workdir file, whose credential-origin rule (origin.go) is untouched.
var RunEnvCommand = func(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), envCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("timed out after %s", envCommandTimeout)
	}
	if err != nil {
		detail := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			tail := strings.TrimSpace(string(ee.Stderr))
			if len(tail) > 200 {
				tail = tail[len(tail)-200:]
			}
			detail = ": " + tail
		}
		return "", fmt.Errorf("exit %v%s", exitStatus(err), detail)
	}
	return strings.TrimSpace(string(out)), nil
}

// EnvValue is one entry of an `env` map: either a literal string — the JSON
// value IS the string, including the "@path" file-reference form resolveEnv
// always supported — or an object {"fromCommand": "..."} whose stdout becomes
// the value, resolved fresh at every session spawn.
type EnvValue struct {
	// Literal carries a literal value. It may be "@path", which resolves to the
	// contents of that file (owner-only enforced, exactly as before).
	Literal string

	// FromCommand carries the shell command a {"fromCommand": ...} entry runs.
	FromCommand string

	// isCommand distinguishes {"fromCommand": ""} (an empty command, which
	// Validate refuses) from a literal whose value happens to be "".
	isCommand bool
}

// IsCommand reports whether this entry is command-derived.
func (v EnvValue) IsCommand() bool { return v.isCommand }

// CommandEnv returns the {"fromCommand": command} form of an env value, for
// callers that build config in code rather than parse it from JSON.
func CommandEnv(command string) EnvValue {
	return EnvValue{FromCommand: command, isCommand: true}
}

// UnmarshalJSON accepts a JSON string (literal, including "@path") or an object
// carrying "fromCommand". Everything else — numbers, booleans, arrays, an
// object without the key — is refused at parse time with an error naming what
// was found, because a silently-dropped declaration is a session missing its
// declared env, which is precisely the failure this map exists to end.
func (v *EnvValue) UnmarshalJSON(b []byte) error {
	v.Literal, v.FromCommand, v.isCommand = "", "", false
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v.Literal = s
		return nil
	}
	var obj struct {
		FromCommand *string `json:"fromCommand"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("env value must be a string or {\"fromCommand\": \"...\"}, got %s", jsonTypeName(b))
	}
	if obj.FromCommand == nil {
		return fmt.Errorf(`env object must carry "fromCommand", got %s`, jsonTypeName(b))
	}
	v.FromCommand = *obj.FromCommand
	v.isCommand = true
	return nil
}

// MarshalJSON writes the value back in the shape it parsed from, so a
// marshalled config round-trips.
func (v EnvValue) MarshalJSON() ([]byte, error) {
	if v.isCommand {
		return json.Marshal(map[string]string{"fromCommand": v.FromCommand})
	}
	return json.Marshal(v.Literal)
}

// EnvMap is one `env` block: variable name -> value.
type EnvMap map[string]EnvValue

// DeclaredEnv describes one declared env entry, for `doctor`'s verification.
// Values are deliberately absent: doctor's output is the thing an operator
// pastes into an issue, and these entries are routinely credentials.
type DeclaredEnv struct {
	// Label names WHERE the entry was declared, in the config's own spelling
	// ("env", "harnesses.opencode.env", "projects[0].env_per_harness.claude").
	Label string
	// Name is the variable.
	Name string
	// FromCommand is the command a command-derived entry runs; empty when the
	// entry is literal.
	FromCommand string
	// Path is the file an "@path" literal points at (leading ~ unexpanded);
	// empty otherwise. Worth stat-ing: its owner-only rule used to be enforced
	// once at Validate, and now that resolution happens per spawn this is how a
	// preflight still catches a chmod 644 secret before a run leans on it.
	Path string
}

// validateEnvDecls holds one `env` block to its schema: every NAME must be a
// valid environment-variable name, and every declared command must be
// non-empty. Refused at Validate rather than discovered at spawn, because both
// failures are static facts of the config file.
//
// The name rule is also how "non-string names" are refused. encoding/json
// delivers map keys as Go strings no matter what the file said, so a bare
// number in the file fails JSON parsing outright and a quoted number arrives
// here as the string "123" — which is not a name any child process can read,
// and is refused by the same rule that catches "A-B" and "".
func validateEnvDecls(m EnvMap, label string) error {
	for _, name := range sortedKeys(m) {
		if !validEnvName(name) {
			return fmt.Errorf("%s: %q is not a valid environment-variable name ([A-Za-z_][A-Za-z0-9_]*)", label, name)
		}
		v := m[name]
		if v.IsCommand() && strings.TrimSpace(v.FromCommand) == "" {
			return fmt.Errorf("%s.%s: \"fromCommand\" must not be empty", label, name)
		}
	}
	return nil
}

// validEnvName reports whether name is a portable environment-variable name.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// SessionEnv composes ONE session's extra environment: the four-level overlay
// (top level, then the harness block, then the project, then the project's
// per-harness block — ResolveMCPConfig's precedence order, least specific
// first, each key's most specific declaration winning) with every fromCommand
// run FRESH, right now, so a rotated token reaches the next session without a
// daemon restart.
//
// The error names the VARIABLE whose resolution failed, and callers refuse the
// spawn on it — fail closed, never a silent partial env. Literals need no
// execution but their "@path" files are re-read per spawn too, so a chmodded
// or deleted secret file surfaces the same way instead of surviving until the
// next daemon restart.
//
// Sorted output keeps the child's environment deterministic across otherwise
// identical spawns.
func (c *Config) SessionEnv(harnessName, slug string) ([]string, error) {
	overlaid := make(EnvMap)
	for _, layer := range c.envOverlay(harnessName, slug) {
		for k, v := range layer {
			overlaid[k] = v
		}
	}
	keys := sortedKeys(overlaid)
	if len(keys) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		var val string
		var err error
		if v := overlaid[k]; !v.IsCommand() {
			val, err = resolveLiteral(v.Literal)
		} else {
			val, err = resolveEnvValue(k, overlaid[k])
		}
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		out = append(out, k+"="+val)
	}
	return out, nil
}

// envOverlay returns the four layers for one session, least specific first:
// top level, harness block, project block, project's per-harness block. The
// project layers apply only when slug matches a declared projects[] entry —
// the same lookup ReposFor uses — and an unmatched slug (the single-project
// case, which reaches here with "") gets the top-level and harness layers only.
func (c *Config) envOverlay(harnessName, slug string) []EnvMap {
	layers := []EnvMap{c.Env}
	if hc, ok := c.Harnesses[harnessName]; ok {
		layers = append(layers, hc.Env)
	}
	for i := range c.Projects {
		if c.Projects[i].Slug == slug {
			layers = append(layers, c.Projects[i].Env)
			if ph, ok := c.Projects[i].EnvPerHarness[harnessName]; ok {
				layers = append(layers, ph)
			}
			break
		}
	}
	return layers
}

// resolveEnvValue turns one declared value into the string the child sees.
// Literals go through the same "@path" handling resolveEnv has always applied;
// command-derived values run now and must SUCCEED with non-empty output — an
// empty success is a token source that has stopped issuing tokens, and handing
// the child an empty GH_TOKEN would move the failure from spawn time (loud,
// named) into git/gh deep inside a session (silent, misattributed).
func resolveEnvValue(name string, v EnvValue) (string, error) {
	if !v.IsCommand() {
		return resolveLiteral(v.Literal)
	}
	command := strings.TrimSpace(v.FromCommand)
	out, err := RunEnvCommand(command)
	if err != nil {
		return "", fmt.Errorf("fromCommand %q failed: %w", command, err)
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("fromCommand %q produced no output", command)
	}
	return out, nil
}

// resolveLiteral resolves a literal value, expanding the "@path" form against
// an owner-only file exactly as resolveEnv did. The underlying error — the
// insecure-mode sentinel included — stays wrapped, so callers can still
// classify it.
func resolveLiteral(literal string) (string, error) {
	if !strings.HasPrefix(literal, "@") {
		return literal, nil
	}
	path := expandHome(strings.TrimPrefix(literal, "@"))
	data, err := readOwnerOnly(path, groupOtherAccess)
	if err != nil {
		if errors.Is(err, errInsecureMode) {
			return "", fmt.Errorf("%w - an @path secret must be readable only by you: chmod 600 %s", err, path)
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// DeclaredEnvs lists every env declaration in the config, in a stable order,
// for `doctor`. Command-derived entries are the ones worth executing; "@path"
// literals are worth re-checking under their owner-only rule; plain literals
// are listed so the operator can see the whole picture.
func (c *Config) DeclaredEnvs() []DeclaredEnv {
	var out []DeclaredEnv
	add := func(label string, m EnvMap) {
		for _, name := range sortedKeys(m) {
			d := DeclaredEnv{Label: label, Name: name, FromCommand: m[name].FromCommand}
			if !m[name].IsCommand() && strings.HasPrefix(m[name].Literal, "@") {
				d.Path = strings.TrimPrefix(m[name].Literal, "@")
			}
			out = append(out, d)
		}
	}
	add("env", c.Env)
	for _, name := range sortedKeys(c.Harnesses) {
		add("harnesses."+name+".env", c.Harnesses[name].Env)
	}
	for i := range c.Projects {
		p := &c.Projects[i]
		add(fmt.Sprintf("projects[%d].env", i), p.Env)
		for _, h := range sortedKeys(p.EnvPerHarness) {
			add(fmt.Sprintf("projects[%d].env_per_harness.%s", i, h), p.EnvPerHarness[h])
		}
	}
	return out
}

// VerifyEnvFilePath checks that an "@path" secret file is readable and meets
// its owner-only rule, without returning its contents. It is the check
// Validate used to run on every @path value; it lives here so `doctor` can
// keep making that preflight before a run leans on a file resolution now
// deferred to spawn time.
func VerifyEnvFilePath(path string) error {
	_, err := readOwnerOnly(expandHome(path), groupOtherAccess)
	if err != nil {
		if errors.Is(err, errInsecureMode) {
			return fmt.Errorf("%w - an @path secret must be readable only by you: chmod 600 %s", err, path)
		}
		return err
	}
	return nil
}

// TokenSourceDeclared reports whether ANY declaration applicable to one
// project's sessions sets varName — top-level, any harness block, that
// project's own blocks. `doctor` uses it for the GH_TOKEN warning: a project
// whose repos push needs a token from somewhere, and the wrapper script that
// used to provide one is no longer load-bearing.
func (c *Config) TokenSourceDeclared(slug, varName string) bool {
	for _, layer := range c.envOverlayAllHarnesses(slug) {
		if _, ok := layer[varName]; ok {
			return true
		}
	}
	return false
}

// envOverlayAllHarnesses returns every layer that can reach one project's
// sessions regardless of harness: the top level, EVERY declared harness block,
// and the project's own two blocks. Used by the token-source question, which is
// about the operator having declared a source anywhere, not about one session's
// exact overlay.
func (c *Config) envOverlayAllHarnesses(slug string) []EnvMap {
	layers := []EnvMap{c.Env}
	for _, name := range sortedKeys(c.Harnesses) {
		layers = append(layers, c.Harnesses[name].Env)
	}
	for i := range c.Projects {
		if c.Projects[i].Slug == slug {
			layers = append(layers, c.Projects[i].Env)
			for _, h := range sortedKeys(c.Projects[i].EnvPerHarness) {
				layers = append(layers, c.Projects[i].EnvPerHarness[h])
			}
			break
		}
	}
	return layers
}

// jsonTypeName names the JSON kind of b for parse-error messages.
func jsonTypeName(b []byte) string {
	t := strings.TrimSpace(string(b))
	switch {
	case t == "":
		return "nothing"
	case strings.HasPrefix(t, "{"):
		return "an object"
	case strings.HasPrefix(t, "["):
		return "an array"
	case t[0] == '"':
		return "a string"
	default:
		return fmt.Sprintf("%s (%s)", t, "not a string or object")
	}
}

// exitStatus extracts a process exit code for an error message.
func exitStatus(err error) any {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return err
}
