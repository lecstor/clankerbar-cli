package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// CLA-821. `scripts/report-check.sh`: the step that makes this repo's CI evidence EXIST.
//
// It lives beside workflow_coupling_test.go because that file already reads
// `.github/workflows/ci.yml` as the record of what CI is, and this is the same
// coupling seen from the other end: the workflow is only reporting if it invokes a
// script that actually posts what the plane accepts.
//
// WHY THE SCRIPT IS EXERCISED RATHER THAN ASSERTED FROM THE YAML. Its failure mode is
// silence. If it stops posting, nothing here goes red - the plane's CI arm simply never
// passes, no task auto-advances, and it looks exactly like the state this task was filed
// in ("no CLI workflow posts there today"). So the request the plane will actually
// receive is driven into a local HTTP server, and the "never fails the job" rule is a
// case of its own: the step sits under `if: always()` precisely so a red run still
// reports, and a curl that exited non-zero would turn a green run red while the plane
// held "success" - two surfaces contradicting each other about one run.

const reportScript = "../../scripts/report-check.sh"

// Every variable the script reads. Cleared first so a developer's shell cannot leak in
// (a real CLANKERBAR_REPORT_KEY on the machine would otherwise turn every skip test into
// a live POST at whatever CLANKERBAR_BASE happened to be).
var reportInputs = []string{
	"CLANKERBAR_REPORT_KEY",
	"CLANKERBAR_BASE",
	"GITHUB_REPOSITORY",
	"CHECK",
	"OUTCOME",
	"COMMIT",
	"BRANCH",
	"PR",
}

type receivedReport struct {
	method        string
	path          string
	authorization string
	body          map[string]any
}

type reportRecorder struct {
	mu  sync.Mutex
	got []receivedReport
}

func (r *reportRecorder) all() []receivedReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]receivedReport(nil), r.got...)
}

// startPlane is a one-endpoint server that records what arrived and answers with
// `status`/`reply`. It is a real socket rather than an http.Client seam because the
// thing under test is curl and the script's handling of an HTTP status.
func startPlane(t *testing.T, status int, reply string) (*httptest.Server, *reportRecorder) {
	t.Helper()
	rec := &reportRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the report body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("report body is not JSON (it is jq-built, so it must be): %v; raw=%q", err, raw)
		}
		rec.mu.Lock()
		rec.got = append(rec.got, receivedReport{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			body:          body,
		})
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// requireReportTools skips when the machine cannot run the script at all. The script
// itself treats a missing jq/curl as "skip, nothing reported" - correct in CI, but here
// it would silently turn every assertion into a no-op.
func requireReportTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "jq", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH, so %s cannot be exercised here: %v", tool, reportScript, err)
		}
	}
}

// runReportCheck runs the script with the inherited environment minus every variable it
// reads, then `env` on top (an empty value is SET to empty, which is the fork-PR shape).
func runReportCheck(t *testing.T, env map[string]string) (code int, output string) {
	t.Helper()
	path, err := filepath.Abs(reportScript)
	if err != nil {
		t.Fatalf("resolving %s: %v", reportScript, err)
	}
	cmd := exec.Command("bash", path)
	cmd.Env = filteredReportEnv(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %s: %v", reportScript, err)
		}
		code = exitErr.ExitCode()
	}
	return code, stdout.String() + stderr.String()
}

func filteredReportEnv(overrides map[string]string) []string {
	drop := make(map[string]bool, len(reportInputs))
	for _, name := range reportInputs {
		drop[name] = true
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && drop[name] {
			continue
		}
		env = append(env, entry)
	}
	for name, value := range overrides {
		env = append(env, name+"="+value)
	}
	return env
}

// goodReport is the happy-path input set; individual cases override one field.
func goodReport(base string) map[string]string {
	return map[string]string{
		"CLANKERBAR_REPORT_KEY": "test-report-key",
		"CLANKERBAR_BASE":       base,
		"GITHUB_REPOSITORY":     "lecstor/clankerbar-cli",
		"CHECK":                 "ci",
		"OUTCOME":               "success",
		"COMMIT":                strings.Repeat("a", 40),
		"BRANCH":                "clanker/abc-a-task",
		"PR":                    "821",
	}
}

func TestReportCheckPostsTheReportThePlaneExpects(t *testing.T) {
	requireReportTools(t)
	srv, rec := startPlane(t, http.StatusCreated, "{}")

	code, output := runReportCheck(t, goodReport(srv.URL))
	if code != 0 {
		t.Errorf("exit code = %d, want 0 - the step must never fail the job; output:\n%s", code, output)
	}
	if !strings.Contains(output, "HTTP 201") {
		t.Errorf("output does not say the POST succeeded:\n%s", output)
	}

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("the plane received %d requests, want exactly 1:\n%+v", len(got), got)
	}
	if got[0].method != http.MethodPost || got[0].path != "/api/check-reports" {
		t.Errorf("request was %s %s, want POST /api/check-reports", got[0].method, got[0].path)
	}
	if want := "Bearer test-report-key"; got[0].authorization != want {
		t.Errorf("Authorization = %q, want %q", got[0].authorization, want)
	}
	want := map[string]any{
		"repo":    "lecstor/clankerbar-cli",
		"check":   "ci",
		"outcome": "success",
		"commit":  strings.Repeat("a", 40),
		"branch":  "clanker/abc-a-task",
		"pr":      "#821",
	}
	if !reflect.DeepEqual(got[0].body, want) {
		t.Errorf("report body = %#v, want %#v", got[0].body, want)
	}
}

func TestReportCheckReportsAFailedRunToo(t *testing.T) {
	requireReportTools(t)
	// `if: always()` exists for exactly this case: a red run must say so, because "no
	// report" and "no CI here" are indistinguishable to the plane.
	srv, rec := startPlane(t, http.StatusCreated, "{}")

	env := goodReport(srv.URL)
	env["OUTCOME"] = "failure"
	code, output := runReportCheck(t, env)

	if code != 0 {
		t.Errorf("exit code = %d, want 0; output:\n%s", code, output)
	}
	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("the plane received %d requests, want 1", len(got))
	}
	if got[0].body["outcome"] != "failure" {
		t.Errorf("reported outcome = %v, want %q - absence is not a pass", got[0].body["outcome"], "failure")
	}
}

func TestReportCheckOmitsFieldsItWasNotGiven(t *testing.T) {
	requireReportTools(t)
	// The script is shared with the plane repo, where a push-triggered check has no PR.
	// Dropping an absent field is what keeps a body the endpoint accepts.
	srv, rec := startPlane(t, http.StatusCreated, "{}")

	env := goodReport(srv.URL)
	delete(env, "PR")
	code, output := runReportCheck(t, env)

	if code != 0 {
		t.Errorf("exit code = %d, want 0; output:\n%s", code, output)
	}
	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("the plane received %d requests, want 1", len(got))
	}
	if _, present := got[0].body["pr"]; present {
		t.Errorf("body carries a `pr` that was never supplied: %#v", got[0].body)
	}
	if got[0].body["branch"] != "clanker/abc-a-task" {
		t.Errorf("branch = %v, want it still reported", got[0].body["branch"])
	}
}

func TestReportCheckSkipsWithALogLineWhenTheKeyIsEmpty(t *testing.T) {
	requireReportTools(t)
	// A fork PR never receives repository secrets, and a repo that has not minted a
	// report-only key yet has none either. Both are normal, so both look like this.
	srv, rec := startPlane(t, http.StatusCreated, "{}")

	env := goodReport(srv.URL)
	env["CLANKERBAR_REPORT_KEY"] = ""
	code, output := runReportCheck(t, env)

	if code != 0 {
		t.Errorf("exit code = %d, want 0; output:\n%s", code, output)
	}
	if !strings.Contains(output, "CLANKERBAR_REPORT_KEY is empty") || !strings.Contains(output, "skipping") {
		t.Errorf("skip is not a log line a human can find:\n%s", output)
	}
	if got := rec.all(); len(got) != 0 {
		t.Errorf("posted %d report(s) with no key: %+v", len(got), got)
	}
}

func TestReportCheckSkipsWhenTheReportWouldBeAboutNothing(t *testing.T) {
	requireReportTools(t)
	// The endpoint 400s a report naming neither branch nor PR, so the script says why in
	// the log instead of posting a body it knows will be refused.
	srv, rec := startPlane(t, http.StatusCreated, "{}")

	env := goodReport(srv.URL)
	delete(env, "BRANCH")
	delete(env, "PR")
	code, output := runReportCheck(t, env)

	if code != 0 {
		t.Errorf("exit code = %d, want 0; output:\n%s", code, output)
	}
	if !strings.Contains(output, "neither BRANCH nor PR") {
		t.Errorf("skip does not name the reason:\n%s", output)
	}
	if got := rec.all(); len(got) != 0 {
		t.Errorf("posted %d report(s) about nothing: %+v", len(got), got)
	}
}

func TestReportCheckNeverFailsTheJobWhenThePlaneRefuses(t *testing.T) {
	requireReportTools(t)
	// The realistic refusal is a project key pasted into the secret: the endpoint answers
	// 403 report_only_key_required, and that message has to reach the log rather than
	// leave someone hunting for a typo.
	srv, _ := startPlane(t, http.StatusForbidden,
		`{"error":{"code":"report_only_key_required","message":"…"}}`)

	code, output := runReportCheck(t, goodReport(srv.URL))
	if code != 0 {
		t.Errorf("exit code = %d, want 0 - a refusal must not turn the check red; output:\n%s", code, output)
	}
	for _, want := range []string{"::warning", "403", "report_only_key_required"} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q, so the refusal is invisible:\n%s", want, output)
		}
	}
}

func TestReportCheckNeverFailsTheJobWhenThePlaneIsUnreachable(t *testing.T) {
	requireReportTools(t)
	// Nothing listens on port 1. A green CI run must not go red because the plane had a
	// bad minute - the task simply stays in review for a human, which is the safe side.
	code, output := runReportCheck(t, goodReport("http://127.0.0.1:1"))
	if code != 0 {
		t.Errorf("exit code = %d, want 0 - an unreachable plane must not fail the check; output:\n%s", code, output)
	}
	if !strings.Contains(output, "::warning") || !strings.Contains(output, "failed") {
		t.Errorf("an unreachable plane is not reported as a warning:\n%s", output)
	}
}

// The wiring is pinned as text, the way workflow_coupling_test.go pins the job name: the
// head-sha expression is the one detail whose absence is invisible (a report keyed to the
// merge ref matches no task), and comment lines are stripped first because ci.yml's header
// quotes the very strings these assertions look for.
func TestCIWorkflowReportsTheCheckOutcomeToThePlane(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read %s: %v", path, err)
	}
	yaml := stripYAMLComments(string(raw))

	const runLine = "run: scripts/report-check.sh"
	at := strings.Index(yaml, runLine)
	if at < 0 {
		t.Fatalf("%s no longer runs scripts/report-check.sh, so this repo reports no CI "+
			"evidence and the plane's CI arm can never pass", path)
	}
	start := strings.LastIndex(yaml[:at], "- name:")
	if start < 0 {
		t.Fatalf("the script at offset %d is not invoked from a named step in %s", at, path)
	}
	step := yaml[start:at]

	for _, want := range []string{
		// Always, so a FAILED run reports too - "no report" must not look like "no CI".
		"if: always()",
		// Pull requests only: a push to main/staging names a branch no task carries.
		"github.event_name == 'pull_request'",
		"continue-on-error: true",
		"CLANKERBAR_REPORT_KEY: ${{ secrets.CLANKERBAR_REPORT_KEY }}",
		"CHECK: ci",
		"OUTCOME: ${{ job.status }}",
		// `github.sha` on a `pull_request` event is `refs/pull/<n>/merge` - a commit the
		// branch does not have, so a report keyed to it belongs to no task.
		"COMMIT: ${{ github.event.pull_request.head.sha }}",
		"BRANCH: ${{ github.event.pull_request.head.ref }}",
		"PR: ${{ github.event.pull_request.number }}",
	} {
		if !strings.Contains(step, want) {
			t.Errorf("the reporting step in %s is missing %q. The step is:\n%s", path, want, step)
		}
	}
	if strings.Contains(step, "COMMIT: ${{ github.sha }}") {
		t.Errorf("the reporting step keys the report to github.sha, which on a pull_request "+
			"event is the synthetic merge ref - no task can match it. Use the PR head sha. "+
			"The step is:\n%s", step)
	}

	// LAST step of the job, so the outcome it reports is the gate's own. A step added
	// below it would report `job.status` before that step had run.
	if rest := yaml[at+len(runLine):]; strings.Contains(rest, "\n      - ") {
		t.Errorf("the reporting step is not the LAST step of the ci job in %s: something "+
			"was added below it, so job.status is read before that step runs", path)
	}
}

func TestReportCheckScriptIsExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bits are not a thing on Windows")
	}
	info, err := os.Stat(reportScript)
	if err != nil {
		t.Fatalf("stat %s: %v", reportScript, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable (mode %v), and ci.yml invokes it directly as "+
			"`run: scripts/report-check.sh` - the step would fail with 'Permission denied' "+
			"on every PR", reportScript, info.Mode().Perm())
	}
}

// stripYAMLComments drops whole-line comments. Trailing `# ...` suffixes are left alone:
// nothing this file asserts is quoted after code on the same line, and rewriting YAML
// quoting rules to find out would be a parser, not a guard.
func stripYAMLComments(yaml string) string {
	lines := strings.Split(yaml, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// Ensure a future edit to filteredReportEnv cannot silently stop clearing an input: the
// compile-time-ish check is cheap, and the alternative is a test that POSTs with whatever
// key the developer's shell exported.
func TestReportInputsMatchTheScript(t *testing.T) {
	raw, err := os.ReadFile(reportScript)
	if err != nil {
		t.Fatalf("read %s: %v", reportScript, err)
	}
	script := string(raw)
	for _, name := range reportInputs {
		if !strings.Contains(script, name) {
			t.Errorf("reportInputs lists %s, which %s no longer reads - update the list "+
				"so the tests keep clearing the environment the script actually consults", name, reportScript)
		}
	}
}
