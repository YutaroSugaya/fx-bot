package advisor

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseReflection turns the reflection-analyst subagent stdout into (newRules, update). FAIL-SAFE:
// any empty / garbled / update:false / empty-rules output returns ("", false) = keep the current
// playbook (a safe no-op). The reflection loop only ever swaps advisory text; on bad output it
// changes nothing. Reuses the same messy-output cleanup as the other parsers.
func ParseReflection(stdout []byte) (newRules string, update bool) {
	if len(strings.TrimSpace(string(stdout))) == 0 {
		return "", false
	}
	s := spaceEntityRE.ReplaceAllString(string(stdout), " ")
	s = colonNoSpaceRE.ReplaceAllString(s, "$1: $2")
	if m := fenceBlockRE.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) != "" {
		s = m[1]
	} else {
		s = openFenceRE.ReplaceAllString(s, "")
		s = strings.ReplaceAll(s, "```", "")
	}

	var raw struct {
		Update  bool   `yaml:"update"`
		RulesJP string `yaml:"rules_jp"`
	}
	if err := yaml.Unmarshal([]byte(s), &raw); err != nil {
		return "", false
	}
	rules := strings.TrimSpace(raw.RulesJP)
	if !raw.Update || rules == "" {
		return "", false
	}
	return rules, true
}
