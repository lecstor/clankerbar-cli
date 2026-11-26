package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// opencodeTruncatedOutput returns the full text of a tool result 2.x truncated (see
// opencodeToolState), ok=false when the result was not truncated. Any failure is returned as an
// error; the caller fails safe.
func opencodeTruncatedOutput(st *opencodeToolState, dir string) (string, bool, error) {
	if st.Metadata == nil || st.Metadata.Metadata == nil || !st.Metadata.Metadata.Truncated {
		return "", false, nil
	}
	path := st.Metadata.Metadata.OutputPath
	if path == "" || !filepath.IsAbs(path) {
		return "", false, fmt.Errorf("no absolute outputPath in the truncation metadata (%q)", path)
	}
	if dir == "" {
		return "", false, fmt.Errorf("the session's tool-output directory is unknown")
	}
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false, fmt.Errorf("tool-output directory %s: %w", dir, err)
	}
	gotDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || gotDir != wantDir {
		return "", false, fmt.Errorf("%s is not in this session's tool-output directory %s", path, dir)
	}
	if !strings.HasPrefix(filepath.Base(path), "tool_") {
		return "", false, fmt.Errorf("%s is not an opencode tool-output file", path)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", false, err
	}
	if !fi.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := readSpilled(path)
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// opencodeToolOutputDir is <data home>/opencode/tool-output for the session in: XDG_DATA_HOME, else
// HOME/.local/share, each read from the session's own environment (in.Env wins over the inherited
// one, as it does on exec).
func opencodeToolOutputDir(in Invocation) string {
	get := func(k string) string {
		v := os.Getenv(k)
		for _, kv := range in.Env {
			if name, val, ok := strings.Cut(kv, "="); ok && name == k {
				v = val
			}
		}
		return v
	}
	data := get("XDG_DATA_HOME")
	if data == "" {
		home := get("HOME")
		if home == "" {
			return ""
		}
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "opencode", "tool-output")
}

// opencodeClankerbarTool maps an opencode tool name onto the namespaced name the shared observer
// switches on, and returns "" for anything that is not a clankerbar tool. The server name
// `clankerbar` is HARD-CODED, here as in claude's constants, and that is an assumption the driver
// does not enforce: it never writes an MCP config (the operator hand-writes opencode's, per the
// README), and config.Validate deliberately accepts a clankerbar server under any key so long as it
// is handed CLANKERBAR_API_KEY.
func opencodeClankerbarTool(name string) string {
	const prefix = "clankerbar_"
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	return "mcp__clankerbar__" + strings.TrimPrefix(name, prefix)
}
