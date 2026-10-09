'use client'

import { useEffect, useRef, useState } from 'react'
import {
  createChart,
  CandlestickSeries,
  LineSeries,
  ColorType,
  CrosshairMode,
  type IChartApi,
  type ISeriesApi,
  type CandlestickData,
  type LineData,
  type UTCTimestamp,
} from 'lightweight-charts'
import { priceDecimals } from '../lib/price_format'

export type ChartCandle = {
  time?: number // UNIX seconds (UTC), bar open time
  open: number
  high: number
  low: number
  close: number
}

// TF interval in seconds — used only to synthesize timestamps when the backend
// hasn't sent `time` yet (e.g. an older bot build is still running). Once the
// bot is restarted with the new build, real `time` values are used.
const TF_SECONDS: Record<string, number> = {
  '1MIN': 60,
  '5M': 5 * 60,
  '30M': 30 * 60,
  '1H': 60 * 60,
  '1D': 24 * 60 * 60,
  '1M': 30 * 24 * 60 * 60,
  '3M': 90 * 24 * 60 * 60,
}

// DB/GMO times are UTC. The dashboard shows JST throughout, so we shift the axis by +9h
// to render JST wall-clock. lightweight-charts has no native timezone support;
// offsetting the timestamp is the standard trick.
const JST_OFFSET = 9 * 60 * 60

function priceFormatForSymbol(symbol: string) {
  const dp = priceDecimals(symbol)
  return { type: 'price' as const, precision: dp, minMove: 1 / 10 ** dp }
}

function buildData(candles: ChartCandle[], tfName: string, nowSec: number): CandlestickData[] {
  const step = TF_SECONDS[tfName] ?? 60
  const n = candles.length
  return candles.map((c, i) => {
    const base = c.time && c.time > 0 ? c.time : nowSec - (n - 1 - i) * step
    return {
      time: (base + JST_OFFSET) as UTCTimestamp,
      open: c.open,
      high: c.high,
      low: c.low,
      close: c.close,
    }
  })
}

/**
 * TradingChart renders a single timeframe as a full TradingView-style
 * candlestick chart (price axis, time axis, grid, crosshair, OHLC legend).
 * The chart instance is created once and reused; data is pushed on prop change.
 */
// maLineData zips a per-candle MA array (0 = undefined) with the candle times
// already computed for the candlestick series, dropping the undefined points.
function maLineData(candleData: CandlestickData[], ma?: number[]): LineData[] {
  if (!ma || ma.length === 0) return []
  const out: LineData[] = []
  for (let i = 0; i < candleData.length && i < ma.length; i++) {
    if (ma[i] && ma[i] > 0) out.push({ time: candleData[i].time, value: ma[i] })
  }
  return out
}

export function TradingChart({
  candles,
  tfName,
  symbol,
  height = 380,
  maSma,
  maEma,
}: {
  candles: ChartCandle[]
  tfName: string
  symbol: string
  height?: number
  // Optional 200-period MA overlays, aligned 1:1 with `candles` (0 = undefined).
  maSma?: number[]
  maEma?: number[]
}) {
  const containerRef = useRef<HTMLDivElement | null>(null)
  const chartRef = useRef<IChartApi | null>(null)
  const seriesRef = useRef<ISeriesApi<'Candlestick'> | null>(null)
  const smaRef = useRef<ISeriesApi<'Line'> | null>(null)
  const emaRef = useRef<ISeriesApi<'Line'> | null>(null)
  const [legend, setLegend] = useState<ChartCandle | null>(null)
  const hasMA = (maSma?.some((v) => v > 0) ?? false) || (maEma?.some((v) => v > 0) ?? false)

  // Create the chart + series exactly once.
  useEffect(() => {
    const el = containerRef.current
    if (!el) return
    // Keep the library's default TradingView attribution (required by the lightweight-charts
    // license) — do not set attributionLogo: false.
    const chart = createChart(el, {
      autoSize: true,
      layout: {
        background: { type: ColorType.Solid, color: '#0f1115' },
        textColor: '#9aa0ad',
        fontSize: 11,
      },
      grid: {
        vertLines: { color: '#191d24' },
        horzLines: { color: '#191d24' },
      },
      rightPriceScale: { borderColor: '#262a33' },
      timeScale: { borderColor: '#262a33', timeVisible: true, secondsVisible: false },
      crosshair: { mode: CrosshairMode.Normal },
    })
    // Japanese convention: 陽線 (up) = RED, 陰線 (down) = BLUE (the inverse of the
    // Western teal/red).
    const series = chart.addSeries(CandlestickSeries, {
      upColor: '#ef5350',
      downColor: '#3b82f6',
      borderUpColor: '#ef5350',
      borderDownColor: '#3b82f6',
      wickUpColor: '#ef5350',
      wickDownColor: '#3b82f6',
    })
    // 200MA overlays: orange = 200SMA, blue = 200EMA. Created up front and
    // fed (or cleared) on data change. Thin, no price line / last-value clutter.
    const smaLine = chart.addSeries(LineSeries, {
      color: '#e0a64a', lineWidth: 2, priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false,
    })
    const emaLine = chart.addSeries(LineSeries, {
      color: '#4a9fe0', lineWidth: 2, priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false,
    })
    chart.subscribeCrosshairMove((param) => {
      const d = param.seriesData.get(series) as CandlestickData | undefined
      if (d && typeof d.open === 'number') {
        setLegend({ open: d.open, high: d.high, low: d.low, close: d.close })
      } else {
        setLegend(null)
      }
    })
    chartRef.current = chart
    seriesRef.current = series
    smaRef.current = smaLine
    emaRef.current = emaLine
    return () => {
      chart.remove()
      chartRef.current = null
      seriesRef.current = null
      smaRef.current = null
      emaRef.current = null
    }
  }, [])

  // Keep price formatting in sync with the symbol's pip precision.
  useEffect(() => {
    seriesRef.current?.applyOptions({ priceFormat: priceFormatForSymbol(symbol) })
  }, [symbol])

  // Push data on every candles / timeframe change. Wrapped in try/catch so a
  // transient malformed tick (e.g. an out-of-order/duplicate timestamp slipping
  // through) can never throw inside the effect and leave the chart blank — it
  // keeps the last good frame instead. Only fit the view on a timeframe switch
  // (not every 5s poll) so the chart doesn't reset/flicker on each refresh.
  const lastTfRef = useRef<string>('')
  useEffect(() => {
    const series = seriesRef.current
    const chart = chartRef.current
    if (!series || !chart || candles.length === 0) return
    try {
      const data = buildData(candles, tfName, Math.floor(Date.now() / 1000))
      series.setData(data)
      smaRef.current?.setData(maLineData(data, maSma))
      emaRef.current?.setData(maLineData(data, maEma))
      if (lastTfRef.current !== tfName) {
        chart.timeScale().fitContent()
        lastTfRef.current = tfName
      }
    } catch (e) {
      // Keep the previous frame rather than break the chart on a bad tick.
      console.error('[TradingChart] setData failed', tfName, e)
    }
    setLegend(null)
  }, [candles, tfName, maSma, maEma])

  const dp = priceDecimals(symbol)
  const legendUp = legend ? legend.close >= legend.open : true

  return (
    <div style={{ position: 'relative', width: '100%' }}>
      <div ref={containerRef} style={{ width: '100%', height }} />
      {hasMA && (
        <div
          style={{
            position: 'absolute', top: 8, right: 10, display: 'flex', gap: 10,
            padding: '3px 8px', background: 'rgba(15,17,21,0.78)', border: '1px solid #262a33',
            borderRadius: 4, fontSize: 11, pointerEvents: 'none',
          }}
        >
          <span style={{ color: '#e0a64a' }}>― 200SMA</span>
          <span style={{ color: '#4a9fe0' }}>― 200EMA</span>
        </div>
      )}
      {legend && (
        <div
          style={{
            position: 'absolute',
            top: 8,
            left: 10,
            display: 'flex',
            gap: 10,
            padding: '4px 8px',
            background: 'rgba(15,17,21,0.78)',
            border: '1px solid #262a33',
            borderRadius: 4,
            fontSize: 11,
            color: legendUp ? '#ef5350' : '#3b82f6',
            pointerEvents: 'none',
            fontVariantNumeric: 'tabular-nums',
          }}
        >
          <span><span style={{ opacity: 0.6 }}>O</span> {legend.open.toFixed(dp)}</span>
          <span><span style={{ opacity: 0.6 }}>H</span> {legend.high.toFixed(dp)}</span>
          <span><span style={{ opacity: 0.6 }}>L</span> {legend.low.toFixed(dp)}</span>
          <span><span style={{ opacity: 0.6 }}>C</span> {legend.close.toFixed(dp)}</span>
        </div>
      )}
    </div>
  )
}
