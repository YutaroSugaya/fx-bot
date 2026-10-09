package advisor

import "regexp"

// cliOutcome classifies a claude CLI stdout payload into the control outcome the
// callers branch on. The claude CLI prints its fatal reasons (rate limits, overload,
// usage caps, "not logged in") to STDOUT — on both exit 0 and non-zero exit — so the
// caller must read STDOUT, not just the exit code, to decide whether to retry.
type cliOutcome int

const (
	cliOutcomeNormal     cliOutcome = iota // parse the output / honour the exit code normally
	cliOutcomeTransient                    // retryable: infra blip (529/overload/connection) OR a server-side rate limit
	cliOutcomeUsageLimit                   // hard usage/session cap — do NOT retry (it resets hours later)
)

// serverRateLimitRE matches a SERVER-SIDE rate limit (HTTP 429), which is TRANSIENT
// and self-heals in seconds. It must be distinguished from a usage/session cap.
//
// For example the message
//
//	"API Error: Server is temporarily limiting requests (not your usage limit) · Rate limited"
//
// contains the substring "usage limit" (inside the disambiguating phrase "not your
// usage limit"), so usageLimitRE alone would match it and give up with NO retry.
// This regex catches the rate-limit phrasing FIRST so it routes to the retry path,
// not the give-up path.
var serverRateLimitRE = regexp.MustCompile(`(?i)temporarily limiting requests|rate.?limit|not your usage limit|too many requests|\b429\b`)

// classifyCLIOutput is the single source of truth for both call sites (llm_decision_cli.go
// and claude_cli.go). Order matters: a server rate-limit is checked BEFORE the usage cap
// so the "not your usage limit" disambiguator wins over a naive "usage limit" substring.
func classifyCLIOutput(out string) cliOutcome {
	switch {
	case serverRateLimitRE.MatchString(out):
		return cliOutcomeTransient
	case usageLimitRE.MatchString(out):
		return cliOutcomeUsageLimit
	case transientCLIErrorRE.MatchString(out):
		return cliOutcomeTransient
	default:
		return cliOutcomeNormal
	}
}
