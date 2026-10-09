package config

import "time"

// LoadTimezoneOrUTC は IANA タイムゾーン名 (例 "Asia/Tokyo") から
// *time.Location を返す。空文字や読めない名前は UTC に fallback する。
//
// 「daily window 起点を JST 0:00 に揃える」用途で worker.accountSnapshot,
// worker.SnapshotForAdvisor, main.bootstrapCandles の 3 箇所で同じパターンが
// 重複していた。fallback は UTC が常に安全 (起点がズレるだけで panic しない)。
func LoadTimezoneOrUTC(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil || loc == nil {
		return time.UTC
	}
	return loc
}

// StartOfDayIn は t の「tz name から見たカレンダー日」の 0:00:00 を返す。
// name は LoadTimezoneOrUTC 経由で解決し、空文字・不正名は UTC に fallback する。
//
// 「daily window 起点を tz 0:00 に揃える」用途で worker.accountSnapshot /
// worker.SnapshotForAdvisor / promotion_state / entry_admission の 4 箇所で
// loc 取得 + time.Date(...,0,0,0,0,loc) が重複していた統合先。
func StartOfDayIn(t time.Time, name string) time.Time {
	loc := LoadTimezoneOrUTC(name)
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}
