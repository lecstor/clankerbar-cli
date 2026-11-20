package harness

import (
	"regexp"
	"strings"
)

// opencodeSubscriptionLimitRe matches a provider saying the SUBSCRIPTION's own
// limit is spent, e.g. Synthetic's "You've exceeded your subscription rate
// limits". opencode types that `provider.rate-limit`, the same as a burst 429,
// so the type cannot tell them apart; the wording can. A burst limit names no
// plan ("Rate limit reached for requests"), so this stays narrow on purpose.
var opencodeSubscriptionLimitRe = regexp.MustCompile(`(?i)\bsubscription(?:'s)?\s+(?:rate|usage)\s+limits?\b`)

// opencodeSubscriptionLimit is a usage limit, not a transient: retrying repeats
// the refusal, and only a limit marks the route and moves the session on.
func opencodeSubscriptionLimit(f opencodeErrorFacts, text string) bool {
	rateLimited := f.Status == 429 || strings.EqualFold(f.Code, "RATE_LIMITED")
	return rateLimited && opencodeSubscriptionLimitRe.MatchString(text)
}
