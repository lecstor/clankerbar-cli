package reviewmap

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// maxUses caps the use sites shown per definition.
const maxUses = 8

// useGlobs limits git grep to the sources a use is worth reporting from:
// declarations and minified bundles are never call sites.
var useGlobs = []string{
	"*.ts", "*.tsx", "*.js", "*.jsx", "*.mjs", "*.cjs",
	":!*.d.ts", ":!*.min.js",
}

var (
	testDirRE  = regexp.MustCompile(`(^|/)(tests?|__tests__|e2e|spec)/`)
	testFileRE = regexp.MustCompile(`\.(test|spec)\.[jt]sx?`)
)

// grepUses lists where d is used, or "nowhere else". git is run with argv,
// never a shell.
func grepUses(worktree string, d Definition) (string, error) {
	args := append([]string{"grep", "-n", "-w", "-I", "-e", d.Name, "--"}, useGlobs...)
	cmd := exec.Command("git", args...)
	cmd.Dir = worktree
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "nowhere else", nil
		}
		return "", fmt.Errorf("git grep %q: %w", d.Name, err)
	}
	return formatUses(string(out), d.At), nil
}

// formatUses drops the definition's own line, orders non-test sites before
// test sites, and applies the display cap.
func formatUses(grepOut, defAt string) string {
	defPath, defLine := splitLocation(defAt)
	var nonTest, test []string
	for _, line := range strings.Split(strings.TrimSuffix(grepOut, "\n"), "\n") {
		if line == "" {
			continue
		}
		// git grep -n prints "path:line:text"; the text may contain colons.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		if parts[0] == defPath && parts[1] == defLine {
			continue
		}
		loc := parts[0] + ":" + parts[1]
		if isTestPath(parts[0]) {
			test = append(test, loc)
		} else {
			nonTest = append(nonTest, loc)
		}
	}

	uses := append(nonTest, test...)
	if len(uses) == 0 {
		return "nowhere else"
	}
	more := ""
	if len(uses) > maxUses {
		more = fmt.Sprintf(" (+%d more)", len(uses)-maxUses)
		uses = uses[:maxUses]
	}
	return strings.Join(uses, ", ") + more
}

func splitLocation(at string) (path, line string) {
	at = strings.TrimSuffix(at, " (body changed)")
	i := strings.LastIndex(at, ":")
	if i < 0 {
		return at, ""
	}
	return at[:i], at[i+1:]
}

func isTestPath(path string) bool {
	return testDirRE.MatchString(path) || testFileRE.MatchString(path)
}
