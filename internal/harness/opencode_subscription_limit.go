package harness

import (
	"regexp"
	"strings"
)

// opencodeSubscriptionLimitRe matches a provider saying its SUBSCRIPTION limit
// is spent; opencode types that the same as a burst 429, which names no plan.
var opencodeSubscriptionLimitRe = regexp.MustCompile(`(?i)\bsubscription(?:'s)?\s+(?:rate|usage)\s+limits?\b`)

// opencodeSubscriptionLimit is a usage limit, not a transient: retrying repeats
// the refusal, and only a limit marks the route and moves the session on.
func opencodeSubscriptionLimit(f opencodeErrorFacts, text string) bool {
	rateLimited := f.Status == 429 || strings.EqualFold(f.Code, "RATE_LIMITED")
	return rateLimited && opencodeSubscriptionLimitRe.MatchString(text)
}
