package app

import (
	"testing"

	"fx-bot/backend/internal/config"
)

// Trading at SIZE (effective qty >= 10000) in Live
// mode with the loss-streak guards disabled or no after-loss cooldown is a
// structural blow-up risk. At screening size (qty 1000) guards-off is an
// intentional operating knob (e.g. to collect more samples), so the refusal must key off the EFFECTIVE
// qty, not the hard_limits ceiling — otherwise it would brick a qty=1000
// live run on the next restart.
func TestAssertLiveQtyGuards(t *testing.T) {
	cases := []struct {
		name           string
		mode           config.Mode
		qty            int
		guardsOff      bool
		cooldownAfterL int
		wantErr        bool
	}{
		{"paper always ok", config.ModePaperConfig, 100000, true, 0, false},
		{"live small qty guards-off ok (screening)", config.ModeLiveConfig, 1000, true, 0, false},
		{"live size + guards-off refused", config.ModeLiveConfig, 10000, true, 60, true},
		{"live size + no cooldown refused", config.ModeLiveConfig, 10000, false, 0, true},
		{"live size + guards-on + cooldown ok", config.ModeLiveConfig, 10000, false, 60, false},
		{"live just below threshold ok", config.ModeLiveConfig, 9999, true, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AssertLiveQtyGuards(tc.mode, tc.qty, tc.guardsOff, tc.cooldownAfterL)
			if (err != nil) != tc.wantErr {
				t.Errorf("AssertLiveQtyGuards(%s,%d,off=%v,cd=%d) err=%v, wantErr=%v",
					tc.mode, tc.qty, tc.guardsOff, tc.cooldownAfterL, err, tc.wantErr)
			}
		})
	}
}

func TestMaxActiveConfigQuantity(t *testing.T) {
	h := &ActiveConfigHolder{}
	h.Set("USD_JPY", &config.StrategyConfig{Risk: config.ConfigRiskSection{Quantity: 1000}})
	h.Set("EUR_JPY", &config.StrategyConfig{Risk: config.ConfigRiskSection{Quantity: 5000}})
	if got := MaxActiveConfigQuantity(h); got != 5000 {
		t.Errorf("MaxActiveConfigQuantity = %d, want 5000", got)
	}
	if got := MaxActiveConfigQuantity(&ActiveConfigHolder{}); got != 0 {
		t.Errorf("empty holder = %d, want 0", got)
	}
}
