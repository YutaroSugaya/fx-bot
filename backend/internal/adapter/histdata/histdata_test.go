package histdata

import (
	"strings"
	"testing"
	"time"
)

// HistData.com M1 ASCII format (per https://www.histdata.com/f-a-q/data-files-detailed-specification/):
//   "YYYYMMDD HHMMSS;OpenBid;HighBid;LowBid;CloseBid;Volume"
// Timezone is Eastern Standard Time WITHOUT daylight-saving — i.e. a fixed UTC-5 offset
// year-round. Our candles table stores OpenedAt in UTC, so every bar must be shifted +5h.

func TestParseLine_ESTtoUTC_SameDay(t *testing.T) {
	rec, err := ParseLine("USD_JPY", "20200102 083000;108.000;108.010;107.990;108.005;0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2020, 1, 2, 13, 30, 0, 0, time.UTC) // 08:30 EST + 5h
	if !rec.OpenedAt.Equal(want) {
		t.Errorf("OpenedAt = %s, want %s (EST->UTC must add 5h)", rec.OpenedAt.UTC(), want)
	}
	if rec.OpenedAt.Location() != time.UTC {
		t.Errorf("OpenedAt must be stored in UTC location, got %s", rec.OpenedAt.Location())
	}
	if rec.Symbol != "USD_JPY" {
		t.Errorf("Symbol = %q, want USD_JPY (caller supplies the DB symbol)", rec.Symbol)
	}
	if rec.Timeframe != "1m" {
		t.Errorf("Timeframe = %q, want 1m", rec.Timeframe)
	}
	if rec.Open != 108.000 || rec.High != 108.010 || rec.Low != 107.990 || rec.Close != 108.005 {
		t.Errorf("OHLC mis-parsed: %+v", rec)
	}
	if rec.Volume != 0 {
		t.Errorf("Volume = %v, want 0 (FX has no real volume)", rec.Volume)
	}
}

func TestParseLine_ESTtoUTC_CrossesMidnight(t *testing.T) {
	// 20:00 EST on Jan 2 -> 01:00 UTC on Jan 3 (the +5h shift rolls the date forward).
	rec, err := ParseLine("EUR_USD", "20200102 200000;1.11000;1.11050;1.10950;1.11010;0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2020, 1, 3, 1, 0, 0, 0, time.UTC)
	if !rec.OpenedAt.Equal(want) {
		t.Errorf("OpenedAt = %s, want %s (date must roll forward)", rec.OpenedAt.UTC(), want)
	}
}

func TestParseLine_NoDSTOffsetInSummer(t *testing.T) {
	// HistData uses EST *without* DST, so a July bar is still UTC-5 (not UTC-4).
	// This guards against accidentally using a DST-aware America/New_York zone.
	rec, err := ParseLine("GBP_JPY", "20200701 120000;135.00;135.10;134.90;135.05;0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2020, 7, 1, 17, 0, 0, 0, time.UTC) // +5h, NOT +4h
	if !rec.OpenedAt.Equal(want) {
		t.Errorf("summer OpenedAt = %s, want %s (must stay UTC-5, no DST)", rec.OpenedAt.UTC(), want)
	}
}

func TestParseLine_Malformed(t *testing.T) {
	cases := []string{
		"",                                      // empty
		"20200102 083000;108.0;108.0;108.0",     // too few fields
		"not-a-date;1;2;3;4;0",                  // bad datetime
		"20200102 083000;x;108.0;108.0;108.0;0", // bad open
	}
	for _, c := range cases {
		if _, err := ParseLine("USD_JPY", c); err == nil {
			t.Errorf("ParseLine(%q) = nil error, want error (must fail loud)", c)
		}
	}
}

func TestParseReader_SkipsBlankLines_PreservesOrder(t *testing.T) {
	in := strings.Join([]string{
		"20200102 083000;108.000;108.010;107.990;108.005;0",
		"",
		"20200102 083100;108.005;108.020;108.001;108.018;0",
		"   ",
		"20200102 083200;108.018;108.030;108.010;108.025;0",
	}, "\n")
	recs, err := ParseReader("USD_JPY", strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d bars, want 3 (blank lines skipped)", len(recs))
	}
	if !recs[0].OpenedAt.Before(recs[1].OpenedAt) || !recs[1].OpenedAt.Before(recs[2].OpenedAt) {
		t.Errorf("bars must preserve ascending time order: %v", recs)
	}
}
