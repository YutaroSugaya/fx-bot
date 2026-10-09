package advisor

import (
	"strings"
	"testing"
)

// The reflection loop outputs the FULL revised portfolio, which REPLACES the playbook — so a
// revision could silently drop the human-approved per-currency discipline. The prompt must order
// the panel to preserve the 【HARD禁止】 and 【この通貨の規律】 blocks verbatim: the reflection
// loop may propose tightening, but never relax or delete them (relaxing is a human decision, and
// code vetoes enforce them anyway).
func TestBuildReflectionPayload_PreservesOwnerDisciplineBlocks(t *testing.T) {
	payload := string(BuildReflectionPayload("trades", "PB"))
	for _, want := range []string{"【HARD禁止】", "【この通貨の規律】", "verbatim"} {
		if !strings.Contains(payload, want) {
			t.Errorf("reflection prompt missing discipline-preservation marker %q", want)
		}
	}
	// The prompt must still carry the inputs.
	for _, want := range []string{"=== current_portfolio ===", "=== recent_trades ==="} {
		if !strings.Contains(payload, want) {
			t.Errorf("reflection prompt missing section %q", want)
		}
	}
}
