import type { TimelinePoint, Window } from '../types'

// Per-point x-axis category key for the trend chart. It MUST be unique across
// the whole window: recharts' category axis collapses points that share a key,
// and for a duplicate key it plots the rate Line (and its hover active-dot) at
// the FIRST occurrence while the cursor uses the per-index tick — so the green
// ball drifts away from the vertical cursor (most visible on flat 100% runs).
// HH:MM is unique inside 1h/24h, but in 7d the same hour recurs on different
// days, so 7d carries the date (MM-DD) to disambiguate.
export function trendTickKey(ts: number, win: Window): string {
  const d = new Date(ts * 1000)
  const pad = (n: number) => n.toString().padStart(2, '0')
  const hhmm = `${pad(d.getHours())}:${pad(d.getMinutes())}`
  if (win === '7d') return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${hhmm}`
  return hhmm
}

export interface TrendRow { t: string; req: number; rate: number | null }

export function buildTrendRows(timeline: TimelinePoint[], win: Window): TrendRow[] {
  return (timeline ?? []).map(p => ({
    t: trendTickKey(p.ts, win),
    req: p.req,
    rate: p.req ? (p.ok / p.req) * 100 : null,
  }))
}

// Shorter label printed under the axis. The dataKey stays the full unique key
// (so categories don't collapse); this only trims what's shown to avoid overlap
// — 7d prints the date, the full "MM-DD HH:MM" still appears in the tooltip.
export function trendTickLabel(key: string, win: Window): string {
  if (win === '7d') return key.slice(0, 5) // "MM-DD"
  return key
}
