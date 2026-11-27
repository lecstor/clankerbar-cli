package harness

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// opencodeBudgetRe recognizes a HARD budget/credit-exhaustion stop from the configured provider:
// out of credits, spend-cap or monthly/billing-limit reached, or an HTTP 402 Payment Required. The
// generic wordings that name an exhausted quota without saying which window it is — "usage limit
// reached", "insufficient quota" — live in opencodeGenericLimitRe, because the SAME words arrive
// on a rolling subscription window that a wait can outlast: a provider.quota event whose text is
// "Usage limit reached ...
var opencodeBudgetRe = regexp.MustCompile(`(?i)"status(code)?": ?402` +
	`|payment required` +
	`|out of (credit|credits|balance)` +
	`|insufficient (credit|credits|fund|funds|balance)` +
	`|(credit|token) balance (is )?(too low|depleted)` +
	`|spend[ -]?(cap|limit)|spending limit` +
	`|monthly (usage )?limit|billing (hard )?limit`)

// opencodeGenericLimitRe names an exhausted quota WITHOUT naming a window or a hard condition: the
// legacy "usage limit reached", and "insufficient [provider] quota".
var opencodeGenericLimitRe = regexp.MustCompile(`(?i)usage limit reached` +
	`|insufficient (?:[\w-]+ )?quota`)

// opencodeRollingQuotaRe recognizes the OTHER kind of provider limit: a rolling subscription window
// that a wait can outlast. The Command Code plan's 5-hour quota arrives as an HTTP 429 typed
// `provider.quota` and states the reset instant in its message; the loop supervises that wait and
// re-runs the session once the window lifts, so this must be reported as Limited with Stop UNSET
// rather than falling through to the transient ladder.
var opencodeRollingQuotaRe = regexp.MustCompile(`(?i)"type": ?"provider\.quota"` +
	`|\b\d+\s*[- ]?\s*(hour|hr|minute|min)s?\b[^.\n"]{0,40}\blimit\b` +
	`|\b(?:limit|quota)\b[^.\n"]{0,40}\b\d+\s*[- ]?\s*(hour|hr|minute|min)s?\b` +
	`|\b(session|daily|weekly)\s+usage\s+limit` +
	// CLA-1024: a FREE-TIER cap ("Free usage limit reached") is a recurring rolling limit like any
	// other plan window, even untagged: reading it as a depleted paid account stops the whole run on
	// a cap that lifts. The hard-stop arms above it still win (a monthly cap or a 402 stays Stop).
	`|\bfree\s+(?:tier\s+)?usage\s+limit`)

// opencodeResetRe captures the reset instant a rolling quota STATES as RFC3339, e.g. "Your limit
// resets at 2026-10-20T15:56:48.251Z." When the message does not state one the parse returns the
// zero time and the supervised wait falls back to interval probing — a missing reset must not
// demote the limit to "no limit".
var opencodeResetRe = regexp.MustCompile(`(?i)resets?\s+at\s+` +
	`(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))`)

// opencodeResetInRe captures a RELATIVE reset a rolling quota states by duration, e.g. "Usage limit
// reached. Resets in 4 hours." or "reset in 45m".
var opencodeResetInRe = regexp.MustCompile(`(?i)resets?\s+in\s+(\d+)\s*(hours?|hrs?|minutes?|mins?|h|m)\b`)

func parseOpencodeReset(s string) time.Time { return parseOpencodeResetAt(s, time.Now()) }

// parseOpencodeResetAt is parseOpencodeReset with an injectable "now" for testing — the same
// split parseClaudeResetAt uses.
func parseOpencodeResetAt(s string, now time.Time) time.Time {
	if m := opencodeResetRe.FindStringSubmatch(s); m != nil {
		if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
			return t
		}
	}
	m := opencodeResetInRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return time.Time{}
	}
	switch strings.ToLower(m[2])[0] {
	case 'h':
		return now.Add(time.Duration(n) * time.Hour)
	case 'm':
		return now.Add(time.Duration(n) * time.Minute)
	}
	return time.Time{}
}

func (opencode) DetectLimit(res Result) Limit {
	// Scan only the harness's own diagnostics, NOT the agent's narration: with --format json the
	// assistant's text is a {"type":"text"} part on stdout, so a task that merely discusses billing
	// ("we're out of credits") must not trip a terminal stop.
	text := opencodeErrorText(res)
	// Hard budget stops keep winning: a monthly cap or an exhausted balance has no window to wait out,
	// however the event is typed.
	if opencodeBudgetRe.MatchString(text) {
		return Limit{Limited: true, Stop: true, Reason: "budget_exhausted"}
	}
	// CLA-752: the provider's own STRUCTURED statement comes before its wording.
	facts := opencodeErrorFactsOf(res)
	if window, planLimit := opencodeWindowLimit(facts, text); planLimit {
		return Limit{Limited: true, ResetAt: parseOpencodeReset(text), Reason: "usage_limit", Window: window}
	}
	if opencodeSubscriptionLimit(facts, text) {
		return Limit{Limited: true, ResetAt: parseOpencodeReset(text), Reason: "usage_limit"}
	}
	// A rolling subscription quota is a LIMIT the loop waits out, so Stop stays false.
	if opencodeRollingQuotaRe.MatchString(text) {
		return Limit{Limited: true, ResetAt: parseOpencodeReset(text), Reason: "usage_limit", Window: opencodeNamedWindow(text)}
	}
	// The generic exhausted-quota phrasings are a Stop only when nothing makes the limit waitable.
	if opencodeGenericLimitRe.MatchString(text) {
		if reset := parseOpencodeReset(text); !reset.IsZero() {
			return Limit{Limited: true, ResetAt: reset, Reason: "usage_limit"}
		}
		return Limit{Limited: true, Stop: true, Reason: "budget_exhausted"}
	}
	return Limit{}
}

// opencodeRateLimitWindows maps the provider's structured `rateLimit.window` token onto the label
// the loop reports. Keys are normalised (lower-case, separators stripped) because the provider's
// spelling is not stable: `FiveHour` and `five-hour` are the documented token too, and an
// exact-match map made those variants invisible, which is how a plan cap rode the transient ladder
// (review 1.review-1.4).
var opencodeRateLimitWindows = map[string]string{
	"fivehour": "5-hour",
	"weekly":   "weekly",
	"daily":    "daily",
}

// opencodeWindowToken normalises a structured window token before the closed map lookup: case- and
// separator-insensitive, so `fiveHour`, `five_hour` and `five-hour` are one window rather than one
// known token and two unknown ones.
func opencodeWindowToken(s string) string {
	return strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(strings.TrimSpace(s)))
}

// opencodeLimitWindowPhrase is the plan-limit wording a provider uses when it names no window in
// its body.
const opencodeLimitWindowPhrase = "usage limit for your plan"

// opencodeWindowLimit resolves the plan limit a failure states: the LABEL of the usage window it
// names (empty when it names none this adapter knows), and whether the failure is a plan limit at
// all. Its token is authoritative: when it names a window the closed map does not know, the window
// is UNKNOWN — the wording is not consulted over it, because a window the provider named is not
// something prose may relabel, and the plan's default is a guess the map's own contract forbids
// (reviews 1.review-1.4, 1.review-2.2).
func opencodeWindowLimit(f opencodeErrorFacts, text string) (window string, planLimit bool) {
	rateLimited := f.Status == 429 || strings.EqualFold(f.Code, "RATE_LIMITED")
	if !rateLimited {
		return "", false
	}
	if f.Window != "" {
		// The body NAMED a window. The closed map decides the label — including "no label" for a token
		// it does not know.
		return opencodeRateLimitWindows[opencodeWindowToken(f.Window)], true
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, opencodeLimitWindowPhrase) {
		return "", false
	}
	if window := opencodeNamedWindow(text); window != "" {
		return window, true
	}
	return "5-hour", true
}

// opencodeNamedWindow labels a window the text NAMES, when it names one. The window word only
// counts inside the limit STATEMENT — the same clause as a limit/quota word (either side of it,
// since the provider swaps the order).
var opencodeNamedWindowRe = regexp.MustCompile(`(?i)\b(weekly|daily|5[- ]hour)\b[^.\n"]{0,40}\b(?:usage\s+)?(?:limit|quota)\b` +
	`|\b(?:usage\s+)?(?:limit|quota)\b[^.\n"]{0,40}\b(weekly|daily|5[- ]hour)\b`)

func opencodeNamedWindow(text string) string {
	m := opencodeNamedWindowRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	token := m[1]
	if token == "" {
		token = m[2]
	}
	switch strings.ToLower(token) {
	case "weekly":
		return "weekly"
	case "daily":
		return "daily"
	}
	return "5-hour"
}

// opencodeErrorFacts is what the terminal error event states STRUCTURALLY: fields a caller can
// trust without reading prose.
type opencodeErrorFacts struct {
	// Status is the HTTP status the wrapper reported (error.status, or the older APIError shape's
	// error.data.statusCode).
	Status int
	// Code is the provider body's machine-readable error code, e.g. RATE_LIMITED.
	Code string
	// Window is the raw `rateLimit.window` token, when the body carried one.
	Window string
}

// opencodeErrorFactsOf reads the structured facts out of a finished Result's terminal error event.
// Neither is re-parsed by opencode, so `code` and `rateLimit` are only reachable by decoding that
// string here.
func opencodeErrorFactsOf(res Result) opencodeErrorFacts {
	line := opencodeTerminalErrorLine(res)
	if line == "" {
		return opencodeErrorFacts{}
	}
	var ev map[string]any
	if json.Unmarshal([]byte(line), &ev) != nil {
		return opencodeErrorFacts{}
	}
	errObj := opencodeJSONObject(ev["error"])
	dataObj := opencodeJSONObject(errObj["data"])

	f := opencodeMergeFacts(opencodeFactsFromObject(errObj), opencodeFactsFromObject(dataObj))
	for _, raw := range []any{
		opencodeJSONObject(errObj["response"])["body"],
		dataObj["responseBody"],
		ev["responseBody"],
		errObj["body"],
	} {
		body, ok := raw.(string)
		if !ok || body == "" {
			continue
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(body), &decoded) != nil {
			continue
		}
		f = opencodeMergeFacts(f, opencodeFactsFromObject(decoded))
		// The provider's body is normally an {error:{…}} envelope itself.
		f = opencodeMergeFacts(f, opencodeFactsFromObject(opencodeJSONObject(decoded["error"])))
	}
	return f
}

// opencodeFactsFromObject reads the three structured fields out of one decoded JSON object. Absent
// or wrongly-typed fields stay zero, and a `status` outside the HTTP range is not a status at all
// (review 1.review-1.5).
func opencodeFactsFromObject(o map[string]any) opencodeErrorFacts {
	var f opencodeErrorFacts
	if n, ok := opencodeJSONNumber(o["status"]); ok && n >= 100 && n <= 599 {
		f.Status = n
	} else if n, ok := opencodeJSONNumber(o["statusCode"]); ok && n >= 100 && n <= 599 {
		f.Status = n
	}
	if s, ok := o["code"].(string); ok {
		f.Code = s
	}
	if rl := opencodeJSONObject(o["rateLimit"]); rl != nil {
		if w, ok := rl["window"].(string); ok {
			f.Window = w
		}
	}
	return f
}

// opencodeMergeFacts keeps base's facts and fills only its holes from next.
func opencodeMergeFacts(base, next opencodeErrorFacts) opencodeErrorFacts {
	if base.Status == 0 {
		base.Status = next.Status
	}
	if base.Code == "" {
		base.Code = next.Code
	}
	if base.Window == "" {
		base.Window = next.Window
	}
	return base
}

// opencodeJSONObject is a decoded JSON object, or nil for anything else.
func opencodeJSONObject(v any) map[string]any {
	o, _ := v.(map[string]any)
	return o
}

// opencodeJSONNumber is a decoded JSON number as an int, or false.
func opencodeJSONNumber(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// opencodeErrorText collects the harness-level diagnostic text — all of stderr, plus a TERMINAL
// {"type":"error"} event on stdout — so a limit scan sees provider/transport errors but never the
// agent's own assistant text.
func opencodeErrorText(res Result) string {
	return res.scan(scanErrorText, func() string { return buildOpencodeErrorText(res) })
}

// buildOpencodeErrorText appends the stream's error event ONLY when it is how the session ENDED —
// the last typed event on stdout. stderr is scanned unconditionally: it is the harness's own
// diagnostic channel, written as the process winds down, and the fallback for the untyped failures
// the typed-event scan cannot see.
func buildOpencodeErrorText(res Result) string {
	var b strings.Builder
	b.WriteString(res.Stderr)
	// The error event is appended ONLY when the stream ENDED on it — the rule
	// opencodeTerminalErrorLine owns, shared with the structured fact scan so the two can never
	// disagree about which event is the failure.
	if line := opencodeTerminalErrorLine(res); line != "" {
		b.WriteByte('\n')
		b.WriteString(line)
	}
	return b.String()
}

// opencodeTerminalErrorLine returns the LAST typed event on the stream when it is a
// {"type":"error"} event, verbatim; "" otherwise.
func opencodeTerminalErrorLine(res Result) string {
	last := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type == "" {
			continue
		}
		last = line
	}
	if last == "" {
		return ""
	}
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(last), &ev) != nil || ev.Type != "error" {
		return ""
	}
	return last
}

// opencodeFatalRe: failures that are NEVER a blip, scanned before the transient patterns and before
// the heuristic below so neither can overrule them. A revoked key or a dead MCP server must end the
// run loudly at the first attempt: those are the failures an operator has to act on, and a daemon
// that retries them at the backoff ceiling instead reports "transient failure (exit 1) — retry
// 47" all night and never says what is actually wrong.
const opencodeFatalWords = `(authentication( \w+)? (failed|error|rejected)` +
	`|unauthorized|forbidden|permission denied` +
	`|invalid api key|api key (is )?(invalid|missing|not found|expired))`

// 2.x types its errors, and two of the types are fatal by definition, verified against 2.0.32:
// `provider.auth` (a 401, exit 1, no retry) and `provider.no-route` (an unknown provider or model,
// exit 1, no request sent).
var opencodeFatalRe = regexp.MustCompile(`(?i)"type": ?"provider\.(auth|no-route)"` +
	`|"status(code)?": ?40[13]` +
	`|"message": ?"[^"]*` + opencodeFatalWords +
	`|error: ?[^\n]*` + opencodeFatalWords)

// opencodeTransientRe: retryable server/network blips and API-level rate limits. The Console Go
// tool-schema rejection family (`unsupported_tool_schema` / `tool_count_limit`,
// anomalyco/opencode#43374 and #43378) is classified HERE, deliberately, rather than as a dedicated
// class with its own retry ladder: the gateway's enforcement is INCONSISTENT — #43374 records the
// same catalog accepted and refused seconds apart — so the identical retry often succeeds, which
// is exactly what the transient path does.
var opencodeTransientRe = regexp.MustCompile(`(?i)"type": ?"provider\.rate-limit"` +
	`|"status(code)?": ?(408|429|5\d\d)` +
	`|"isretryable": ?true` +
	`|overloaded|too many requests|rate ?limit` +
	`|unsupported_tool_schema|tool_count_limit` +
	`|"message": ?"transport` +
	`|transport (error|failure|reset|dropped)|transport (is )?clos(ed|ing)|transport-level` +
	`|stream (error|failed|closed|reset|ended|disconnected|interrupted)` +
	`|timed out waiting for response data|sse read timed out` +
	`|connection error|fetch failed|econnreset|econnrefused|etimedout|eai_again|socket hang up|network (error|timeout)`)

func (o opencode) IsTransient(res Result) bool {
	text := opencodeErrorText(res)
	// A named stop wins over every retry path below, including the heuristic.
	if opencodeBudgetRe.MatchString(text) || opencodeRollingQuotaRe.MatchString(text) ||
		opencodeGenericLimitRe.MatchString(text) || opencodeFatalRe.MatchString(text) {
		return false
	}
	// CLA-752: the same veto for a plan cap stated STRUCTURALLY.
	if _, planLimit := opencodeWindowLimit(opencodeErrorFactsOf(res), text); planLimit {
		return false
	}
	// CLA-700: a session that RECOVERED from its provider errors is not retryable, however its exit
	// status reads.
	if RecoveredErrors(res) > 0 {
		return false
	}
	if opencodeTransientRe.MatchString(text) {
		return true
	}
	return o.IsUnclassifiedTransient(res)
}

// IsUnclassifiedTransient is the CLA-381 heuristic, and it is separate from the pattern scan
// because a caller has to be able to bound it — see harness.Adapter.IsUnclassifiedTransient for
// why that is a spending requirement and not a tidiness one. A usage event means the session
// authenticated and started doing paid work, so the failure came after the startup checks and is
// more likely a version-shaped stream drop this adapter has no pattern for than a config/auth
// refusal — that kind fails BEFORE any usage arrives, which is why the no-usage case stays a
// stop.
func (opencode) IsUnclassifiedTransient(res Result) bool {
	if res.ExitCode != 1 || !res.UsageReported {
		return false
	}
	text := opencodeErrorText(res)
	if opencodeBudgetRe.MatchString(text) || opencodeRollingQuotaRe.MatchString(text) ||
		opencodeGenericLimitRe.MatchString(text) || opencodeFatalRe.MatchString(text) {
		return false
	}
	// CLA-752: IsTransient's structural veto, for the same reason — the heuristic must not launder a
	// body-named plan cap into a bounded guess (review 1.review-1.2).
	if _, planLimit := opencodeWindowLimit(opencodeErrorFactsOf(res), text); planLimit {
		return false
	}
	// CLA-700: the same rule as IsTransient's — a session that recovered from its error events has
	// no unrecognised failure to guess at, so the heuristic must not launder its exit status into a
	// retry.
	if RecoveredErrors(res) > 0 {
		return false
	}
	return !opencodeTransientRe.MatchString(text)
}

// Diagnostic returns the scoped text IsTransient's PATTERN scans judged. See the claude adapter's
// Diagnostic for why the scope must match exactly.
func (opencode) Diagnostic(res Result) string { return opencodeErrorText(res) }
