package order

import "testing"

// Side.Opposite() の旧実装は `if s == SideBuy { return SideSell }; return SideBuy`
// で、Side が "" / "FOO" / "buy" など不正な値でも黙って SideBuy を返していた。
// 反対サイドを決める関数で silently BUY を返すのは、不正入力に対して
// 「本番口座での BUY 成行」を呼び出す可能性があり危険。
//
// 修正方針: BUY/SELL 以外は empty Side ("") を返す。empty Side は
// Side.Valid() == false で、下流の PlaceOrder などが弾く設計。
func TestSide_Opposite_ValidPairs(t *testing.T) {
	tests := []struct {
		in   Side
		want Side
	}{
		{SideBuy, SideSell},
		{SideSell, SideBuy},
	}
	for _, tc := range tests {
		if got := tc.in.Opposite(); got != tc.want {
			t.Errorf("Opposite(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestSide_Opposite_InvalidReturnsEmpty(t *testing.T) {
	tests := []Side{
		Side(""),
		Side("FOO"),
		Side("buy"),       // case-sensitive
		Side("SELL_LONG"), // typo
	}
	for _, in := range tests {
		got := in.Opposite()
		if got != Side("") {
			t.Errorf("Opposite(%q): got %q want empty (invalid input must NOT silently become BUY)", in, got)
		}
		if got.Valid() {
			t.Errorf("Opposite(%q).Valid() must be false; got true", in)
		}
	}
}
