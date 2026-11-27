// Package mcpbridge is `clankerbar mcp`: a stdio-to-Streamable-HTTP bridge in
// front of the plane's MCP endpoint. A local harness configures one stdio
// server (`command: "clankerbar"`, `args: ["mcp"]`) and every harness session
// shares the CLI's stored login, so CLANKERBAR_API_KEY never has to be put into
// a harness config or an agent's environment.
package mcpbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/auth"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

const (
	// DefaultOrigin is where the bridge connects when neither --host nor the
	// operator's config names a plane.
	DefaultOrigin = auth.DefaultOrigin

	// projectFileName is the CLA-1021 folder-to-project mapping. It is
	// deliberately NOT clankerbar.json: that file configures the driver and is
	// never discovered from a checkout, while this one exists to be discovered.
	projectFileName = ".clankerbar.json"

	// mcpSurface is the MCP route on every clankerbar origin.
	mcpSurface = "/mcp"
)

// TargetOptions is one target resolution. CWD and Home are injectable so tests
// (and the CLI's own tests) never depend on where the process happens to run.
type TargetOptions struct {
	// Origin is the credential-safe origin (see ResolveOrigin).
	Origin string
	// Project is --project: a slug to pin, "" for none.
	Project string
	// Multi is --multi: use the account-wide /mcp surface.
	Multi bool
	// CWD is where .clankerbar.json discovery starts; "" asks the OS.
	CWD string
	// Home stops the upward walk; "" asks the OS.
	Home string
	// Warn receives the single diagnostic a malformed or unreadable
	// .clankerbar.json produces; nil discards it.
	Warn io.Writer
}

// ResolveOrigin applies the bridge's origin precedence: --host if set, else the
// operator's own config (~/.config/clankerbar/config.json), else the built-in
// plane. The result is always canonical and credential-safe: https anywhere, or
// http to loopback only.
func ResolveOrigin(host string) (string, error) {
	if h := strings.TrimSpace(host); h != "" {
		origin, err := secureurl.Origin(h)
		if err != nil {
			return "", fmt.Errorf("--host: %w", err)
		}
		return origin, nil
	}
	cfg, err := config.LoadHome()
	if err != nil {
		return "", fmt.Errorf("load the config: %w (pass --host <origin> to skip it)", err)
	}
	raw := strings.TrimSpace(cfg.BacklogURL)
	if raw == "" {
		return DefaultOrigin, nil
	}
	origin, err := secureurl.Origin(raw)
	if err != nil {
		return "", fmt.Errorf("the config's backlog_url cannot carry a credential (%w) - pass --host <origin>", err)
	}
	return origin, nil
}

// ResolveTarget turns the flags (and, when neither pins a project, the nearest
// .clankerbar.json) into the Streamable HTTP endpoint to POST to.
//
// The origin is never taken from a file: only the project slug is. An MCP URL
// in a checkout's .mcp.json is not read at all - a repository does not get to
// name where the credential goes.
func ResolveTarget(o TargetOptions) (string, error) {
	origin := strings.TrimRight(strings.TrimSpace(o.Origin), "/")
	if origin == "" {
		return "", errors.New("mcpbridge: no origin to resolve a target against")
	}
	project := strings.TrimSpace(o.Project)
	if o.Multi && project != "" {
		return "", errors.New("--multi and --project are mutually exclusive: --multi uses the account-wide /mcp surface, --project pins one project")
	}
	if o.Multi {
		return origin + mcpSurface, nil
	}
	if project != "" {
		return projectURL(origin, project), nil
	}

	cwd := o.CWD
	if cwd == "" {
		got, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve the working directory: %w", err)
		}
		cwd = got
	}
	home := o.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if slug, ok := discoverProject(cwd, home, o.Warn); ok {
		return projectURL(origin, slug), nil
	}
	// Bare /mcp is the account-wide surface (CLA-1020); a project-scoped stored
	// key already binds it to its project server-side.
	return origin + mcpSurface, nil
}

// projectURL is one project's MCP endpoint, the slug escaped as a single path
// segment so a slug can never add a route.
func projectURL(origin, slug string) string {
	return origin + mcpSurface + "/" + url.PathEscape(slug)
}

// discoverProject walks upward from dir looking for the nearest
// .clankerbar.json that names a project containing dir. It stops after the
// directory carrying .git (a harness's workspace root) and at home or the
// filesystem root, so it never reads above what a sandboxed agent could see.
//
// A malformed or unreadable file produces ONE warning and the bare surface: a
// broken mapping is not a reason to keep guessing at a project.
func discoverProject(dir, home string, warn io.Writer) (string, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	dir = abs
	if home != "" {
		if h, err := filepath.Abs(home); err == nil {
			home = h
		}
	}

	var warned bool
	warnOnce := func(format string, args ...any) {
		if warned || warn == nil {
			return
		}
		warned = true
		fmt.Fprintf(warn, "clankerbar mcp: "+format+"\n", args...)
	}

	for {
		file := filepath.Join(dir, projectFileName)
		data, err := os.ReadFile(file)
		switch {
		case err == nil:
			slug, matched, perr := projectFromFile(data, dir, abs)
			if perr != nil {
				warnOnce("ignoring %s: %v - using the account-wide /mcp", file, perr)
				return "", false
			}
			if matched {
				return slug, true
			}
		case errors.Is(err, os.ErrNotExist):
			// Nothing here; keep walking.
		default:
			warnOnce("ignoring %s: %v - using the account-wide /mcp", file, err)
			return "", false
		}

		if hasGitMarker(dir) || dir == home {
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// clankerbarJSON is the CLA-1021 mapping. The two shapes share one filename: a
// single-project root names `project`, and a multi-project parent maps
// workspace-relative folders in `folders`.
type clankerbarJSON struct {
	Project string            `json:"project"`
	Folders map[string]string `json:"folders"`
}

// projectFromFile resolves one mapping file against cwd. matched is false when
// the file is well-formed but does not cover cwd (a sibling folder of a
// multi-project parent), which is not an error.
func projectFromFile(data []byte, fileDir, cwd string) (slug string, matched bool, err error) {
	var m clankerbarJSON
	if err := json.Unmarshal(data, &m); err != nil {
		return "", false, fmt.Errorf("not valid JSON: %w", err)
	}
	if raw := m.Project; raw != "" {
		p := strings.TrimSpace(raw)
		if p == "" {
			return "", false, errors.New("`project` is empty")
		}
		return p, true, nil
	}
	rel, err := filepath.Rel(fileDir, cwd)
	if err != nil {
		return "", false, err
	}
	rel = filepath.ToSlash(rel)

	bestKey, bestSlug := "", ""
	for key, raw := range m.Folders {
		k := normalizeFolderKey(key)
		if k == "" || !pathPrefix(rel, k) {
			continue
		}
		if len(k) > len(bestKey) {
			bestKey, bestSlug = k, strings.TrimSpace(raw)
		}
	}
	if bestKey == "" {
		return "", false, nil
	}
	if bestSlug == "" {
		return "", false, fmt.Errorf("the folders entry %q names no project", bestKey)
	}
	return bestSlug, true, nil
}

// normalizeFolderKey holds a folders key to a clean forward-slash relative
// path. A key that escapes its own directory is not a mapping this file can
// make, and is skipped rather than matched.
func normalizeFolderKey(key string) string {
	k := strings.TrimSpace(strings.ReplaceAll(key, `\`, "/"))
	k = strings.TrimPrefix(k, "./")
	k = path.Clean(k)
	if k == "." || k == ".." || strings.HasPrefix(k, "../") || strings.HasPrefix(k, "/") {
		return ""
	}
	return k
}

// pathPrefix reports whether rel is key itself or lies below it, judged by path
// segment: "clankerbar" is not a prefix of "clankerbar-cli".
func pathPrefix(rel, key string) bool {
	return rel == key || strings.HasPrefix(rel, key+"/")
}

// hasGitMarker reports whether dir is a checkout root (a worktree's .git is a
// file, so existence is the test, not IsDir).
func hasGitMarker(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
