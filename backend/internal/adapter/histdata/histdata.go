// Package histdata parses HistData.com free M1 (1-minute) forex CSV exports into
// port.CandleRecord rows for the candles table. It exists so the backtest harness can be
// fed pre-2023 history (a yen-STRENGTH / multi-volatility regime) that the GMO 外為 public
// API cannot serve — GMO's forex product only launched 2023-10-28, so the live candles table
// starts there. Without a second regime, JPY-pair backtest "edges" cannot be distinguished
// from harvesting the 2023-26 yen-weakness trend.
//
// HistData M1 ASCII format (https://www.histdata.com/f-a-q/data-files-detailed-specification/):
//
//	"YYYYMMDD HHMMSS;OpenBid;HighBid;LowBid;CloseBid;Volume"
//
// CRITICAL: the timestamp is Eastern Standard Time WITHOUT daylight saving — a FIXED UTC-5
// offset all year. Our candles.opened_at is UTC, so every bar is shifted +5h. Using a
// DST-aware America/New_York zone would misalign summer bars by an hour and break session
// (Tokyo/London) gates. Prices are BID (GMO klines are ASK) — a ~1 pip constant level offset
// at the 2023 seam, harmless for relative-level strategies (MA distance, range breakout).
package histdata

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"fx-bot/backend/internal/port"
)

// estNoDST is HistData's timezone: Eastern Standard Time with NO daylight saving = fixed UTC-5.
var estNoDST = time.FixedZone("EST", -5*60*60)

// ParseLine parses one HistData M1 line into a UTC-normalised CandleRecord. The caller supplies
// the DB symbol (e.g. "USD_JPY"); HistData's own pair naming ("USDJPY") is not used here.
func ParseLine(symbol, line string) (port.CandleRecord, error) {
	fields := strings.Split(strings.TrimSpace(line), ";")
	if len(fields) != 6 {
		return port.CandleRecord{}, fmt.Errorf("histdata: expected 6 ;-separated fields, got %d in %q", len(fields), line)
	}

	// fields[0] = "YYYYMMDD HHMMSS" in EST (no DST). ParseInLocation keeps it naive-in-EST,
	// then .UTC() converts the instant to UTC (adding 5h, rolling the date if needed).
	t, err := time.ParseInLocation("20060102 150405", fields[0], estNoDST)
	if err != nil {
		return port.CandleRecord{}, fmt.Errorf("histdata: bad datetime %q: %w", fields[0], err)
	}

	open, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return port.CandleRecord{}, fmt.Errorf("histdata: bad open %q: %w", fields[1], err)
	}
	high, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return port.CandleRecord{}, fmt.Errorf("histdata: bad high %q: %w", fields[2], err)
	}
	low, err := strconv.ParseFloat(fields[3], 64)
	if err != nil {
		return port.CandleRecord{}, fmt.Errorf("histdata: bad low %q: %w", fields[3], err)
	}
	cl, err := strconv.ParseFloat(fields[4], 64)
	if err != nil {
		return port.CandleRecord{}, fmt.Errorf("histdata: bad close %q: %w", fields[4], err)
	}
	vol, err := strconv.ParseFloat(fields[5], 64)
	if err != nil {
		return port.CandleRecord{}, fmt.Errorf("histdata: bad volume %q: %w", fields[5], err)
	}

	return port.CandleRecord{
		Symbol:    symbol,
		Timeframe: "1m",
		OpenedAt:  t.UTC(),
		Open:      open,
		High:      high,
		Low:       low,
		Close:     cl,
		Volume:    vol,
	}, nil
}

// ParseReader streams a HistData M1 file (one bar per line), skipping blank lines. It fails
// loud on the first malformed bar rather than silently dropping it — a silently-skipped bad
// row would corrupt the regime sample we are building this dataset to provide.
func ParseReader(symbol string, r io.Reader) ([]port.CandleRecord, error) {
	var recs []port.CandleRecord
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	ln := 0
	for sc.Scan() {
		ln++
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		rec, err := ParseLine(symbol, sc.Text())
		if err != nil {
			return nil, fmt.Errorf("histdata: line %d: %w", ln, err)
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("histdata: scan: %w", err)
	}
	return recs, nil
}
