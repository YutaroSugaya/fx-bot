#!/usr/bin/env bash
# fetch_histdata.sh — download HistData.com free M1 (1-minute) yearly CSVs for 5 pairs
# (USD_JPY / EUR_USD / EUR_JPY / GBP_JPY / GBP_USD), 2015-2023, into data/histdata/ for offline
# backtests. This covers the pre-2023 yen-strength / multi-volatility regimes that the GMO 外為 API
# cannot serve (its history starts at the GMO forex launch, 2023-10-28).
#
# This script automates HistData's free download form for personal research use: it GETs the
# download page to read the per-file hidden `tk` token, then POSTs to /get.php (2 requests per
# file, 45 files, 1 s pause between files). Read HistData's terms of use before running it; if they do not allow
# automated downloads, use the manual download below instead. Do NOT redistribute the data
# (data/ is gitignored — never commit or publish the CSVs).
# Alternative / fallback when the form changes: download manually from
# https://www.histdata.com/download-free-forex-historical-data/ (ASCII, 1-minute bar quotes)
# and place the extracted CSVs at the Output path below.
#
# Idempotent: already-extracted CSVs are skipped. Polite 1s gap between requests.
#
# Usage:  bash scripts/fetch_histdata.sh
# Output: data/histdata/DAT_ASCII_<PAIR>_M1_<YEAR>.csv
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=data/histdata
mkdir -p "$OUT"

PAIRS=(USDJPY EURUSD EURJPY GBPJPY GBPUSD)
YEARS=(2015 2016 2017 2018 2019 2020 2021 2022 2023)

base="https://www.histdata.com"
ok=0; skip=0; fail=0

for PAIR in "${PAIRS[@]}"; do
  pair_lc="$(printf '%s' "$PAIR" | tr '[:upper:]' '[:lower:]')"
  for YEAR in "${YEARS[@]}"; do
    csv="$OUT/DAT_ASCII_${PAIR}_M1_${YEAR}.csv"
    if [ -s "$csv" ]; then
      echo "skip  $PAIR $YEAR (already have $csv)"; skip=$((skip+1)); continue
    fi

    ref="$base/download-free-forex-historical-data/?/ascii/1-minute-bar-quotes/${pair_lc}/${YEAR}"
    # Scrape the hidden tk token (attribute order varies, so match tk"...value="..." loosely).
    tk="$(curl -fsSL "$ref" | perl -ne 'if (/name="tk"[^>]*value="([^"]+)"/ || /id="tk"[^>]*value="([^"]+)"/) { print $1; last }' || true)"
    if [ -z "$tk" ]; then
      echo "FAIL  $PAIR $YEAR (no token — HistData form may have changed; download manually)" >&2
      fail=$((fail+1)); sleep 1; continue
    fi

    zip="$OUT/.${PAIR}_${YEAR}.zip"
    if ! curl -fsSL -o "$zip" -X POST "$base/get.php" \
         -H "Referer: $ref" \
         --data "tk=${tk}&date=${YEAR}&datemonth=${YEAR}&platform=ASCII&timeframe=M1&fxpair=${PAIR}"; then
      echo "FAIL  $PAIR $YEAR (download error)" >&2; fail=$((fail+1)); rm -f "$zip"; sleep 1; continue
    fi

    # The zip holds DAT_ASCII_<PAIR>_M1_<YEAR>.csv (+ a .txt readme we ignore).
    if unzip -o -j "$zip" "DAT_ASCII_${PAIR}_M1_${YEAR}.csv" -d "$OUT" >/dev/null 2>&1; then
      rm -f "$zip"
      echo "ok    $PAIR $YEAR -> $csv ($(wc -l < "$csv" | tr -d ' ') bars)"; ok=$((ok+1))
    else
      echo "FAIL  $PAIR $YEAR (unzip — likely an error page, not a zip)" >&2; fail=$((fail+1)); rm -f "$zip"
    fi
    sleep 1
  done
done

echo ""
echo "==> done: ${ok} downloaded, ${skip} skipped, ${fail} failed. Files in $OUT/"
echo "    Next: go run ./backend/cmd/histdata-ingest -dir $OUT      # dry-run (no DB)"
[ "$fail" -eq 0 ]
