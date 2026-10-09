package advisor

import "testing"

// classifyCLIOutput is the single source of truth for routing a claude CLI stdout
// payload to one of: normal / transient (retry) / usage-limit (give up, resets hours later).
//
// The trap: the server-side rate-limit message
//
//	"API Error: Server is temporarily limiting requests (not your usage limit) · Rate limited"
//
// contains the substring "usage limit" (inside the *disambiguating* phrase "not your
// usage limit"), so usageLimitRE-first logic would classify a TRANSIENT 429 as a
// multi-hour usage cap → the decider would give up with NO retry, and every pair would
// show "エラー" on the dashboard. A server rate-limit must route to TRANSIENT (retryable).
func TestClassifyCLIOutput(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want cliOutcome
	}{
		{
			name: "server rate limit explicitly NOT a usage cap → transient",
			out:  "API Error: Server is temporarily limiting requests (not your usage limit) · Rate limited",
			want: cliOutcomeTransient,
		},
		{
			name: "plain rate limited → transient",
			out:  "Rate limited. Please try again shortly.",
			want: cliOutcomeTransient,
		},
		{
			name: "429 too many requests → transient",
			out:  "API Error: 429 Too Many Requests",
			want: cliOutcomeTransient,
		},
		{
			name: "529 overloaded → transient",
			out:  "API Error: 529 Overloaded. This is usually temporary — try again.",
			want: cliOutcomeTransient,
		},
		{
			name: "connection refused → transient",
			out:  "Unable to connect to API (ConnectionRefused)",
			want: cliOutcomeTransient,
		},
		{
			name: "genuine session limit → usage-limit (do NOT retry)",
			out:  "You've hit your session limit · resets 4:20am (Asia/Tokyo)",
			want: cliOutcomeUsageLimit,
		},
		{
			name: "genuine usage limit reached → usage-limit (do NOT retry)",
			out:  "Claude usage limit reached. Your limit will reset at 3pm.",
			want: cliOutcomeUsageLimit,
		},
		{
			name: "normal decision yaml → normal",
			out:  "decision:\n  go: false\nreason_jp: \"static range\"\n",
			want: cliOutcomeNormal,
		},
		{
			name: "empty → normal",
			out:  "",
			want: cliOutcomeNormal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCLIOutput(tc.out); got != tc.want {
				t.Errorf("classifyCLIOutput(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}
