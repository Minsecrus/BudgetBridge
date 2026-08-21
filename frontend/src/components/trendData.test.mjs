// Regression test for the 7d TrendChart bug: duplicate HH:MM category keys
// across days made recharts plot the rate Line's active dot at the FIRST
// occurrence of each hour while the cursor used the per-index tick, so the
// green ball and the vertical cursor diverged (most visible on flat 100% runs).
// Root cause fix: the per-point x-axis key must be unique across the window.
import { test } from 'node:test'
import assert from 'node:assert'
import { trendTickKey, buildTrendRows, trendTickLabel } from './trendData.ts'

// Fixed base instant (UTC) — 2026-06-25T14:00:00Z. Consecutive days at the same
// UTC time = same local time too (no DST transition inside 7 June days), so the
// ONLY thing that should differ across these points is the date — which is what
// the 7d key must encode to stay unique.
const BASE = Math.floor(Date.UTC(2026, 5, 25, 14, 0, 0) / 1000)
const dayTs = (daysAgo) => BASE - daysAgo * 86400

test('7d: same hour on different days produces UNIQUE keys (the bug fix)', () => {
  const keys = [0, 1, 2, 3, 4, 5, 6].map(d => trendTickKey(dayTs(d), '7d'))
  assert.equal(new Set(keys).size, keys.length, `7d keys not unique: ${keys}`)
})

test('7d: two different days at the same hour yield different keys', () => {
  assert.notEqual(trendTickKey(dayTs(0), '7d'), trendTickKey(dayTs(2), '7d'),
    'same hour on different days must differ')
})

test('7d: keys carry the date (MM-DD) so the hour alone is not the category', () => {
  const keys = [0, 1, 2, 3, 4, 5, 6].map(d => trendTickKey(dayTs(d), '7d'))
  for (const k of keys) assert.match(k, /^\d{2}-\d{2} \d{2}:\d{2}$/, `bad 7d key shape: ${k}`)
  const times = new Set(keys.map(k => k.slice(6)))
  const dates = new Set(keys.map(k => k.slice(0, 5)))
  assert.equal(times.size, 1, 'expected same local time across these points')
  assert.equal(dates.size, 7, 'expected 7 distinct dates')
})

test('24h and 1h keep the compact HH:MM key (no date needed, unique within window)', () => {
  assert.match(trendTickKey(dayTs(0), '24h'), /^\d{2}:\d{2}$/)
  assert.match(trendTickKey(dayTs(0), '1h'), /^\d{2}:\d{2}$/)
})

test('buildTrendRows: 7d rows have unique keys and rate maps ok/null', () => {
  const tl = [0, 1, 2].map(d => ({ ts: dayTs(d), req: 4, ok: 4, err: 0, r429: 0, net_retry: 0 }))
  const rows = buildTrendRows(tl, '7d')
  assert.equal(new Set(rows.map(r => r.t)).size, rows.length)
  assert.equal(rows[0].rate, 100)

  const empty = buildTrendRows([{ ts: dayTs(0), req: 0, ok: 0, err: 0, r429: 0, net_retry: 0 }], '7d')
  assert.equal(empty[0].rate, null, 'req=0 must yield null rate, not NaN/0')
})

test('7d axis label trims to the date; full key is retained for the tooltip', () => {
  const key = trendTickKey(dayTs(0), '7d') // e.g. "06-25 14:00"
  assert.equal(trendTickLabel(key, '7d'), key.slice(0, 5))
  assert.equal(trendTickLabel(trendTickKey(dayTs(0), '24h'), '24h'), trendTickKey(dayTs(0), '24h'))
})
