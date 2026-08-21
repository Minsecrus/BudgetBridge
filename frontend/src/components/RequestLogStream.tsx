import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Pause, Play, Trash2, History, Wifi, WifiOff, AlertTriangle } from 'lucide-react'
import { streamEvents, fetchLogs } from '../api'
import type { RequestLog } from '../types'
import type { StreamState } from '../api'

// Auto-pause the live feed after this long so a dashboard left open in a
// background tab doesn't pile up SSE events and lag the browser rendering the
// 2000-row table. 5 minutes = "watch a burst, then let it rest."
const AUTO_PAUSE_MS = 5 * 60 * 1000

const outcomeStyle = (o: string): string => {
  switch (o) {
    case 'ok': return 'text-emerald-400'
    case 'no_accounts': case 'server_error': return 'text-rose-400'
    case 'client_error': return 'text-gray-400'
    case 'throttled': return 'text-amber-400'
    default: return 'text-gray-400'
  }
}

const attemptStyle = (s: number): string => {
  if (s === 200) return 'bg-emerald-500/15 text-emerald-300 border-emerald-500/30'
  if (s === 429) return 'bg-amber-500/15 text-amber-300 border-amber-500/30'
  if (s === 0 || s >= 500) return 'bg-rose-500/15 text-rose-300 border-rose-500/30'
  return 'bg-orange-500/15 text-orange-300 border-orange-500/30'
}

const fmtTime = (ms: number): string => {
  const d = new Date(ms)
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  const ss = String(d.getSeconds()).padStart(2, '0')
  return `${hh}:${mm}:${ss}.${String(ms % 1000).padStart(3, '0')}`
}

// Row is memoized so a new SSE frame (which prepends one entry via prev.slice()
// + unshift, preserving every existing entry's reference) only re-renders the
// single new row, not all 2000 unchanged ones. Every value the row reads comes
// from its `l` prop or a module-level pure helper, so default shallow-prop
// comparison is correct.
const Row = memo(function Row({ l }: { l: RequestLog }) {
  // attempts is [] not null in the wire format (backend normalizes), but guard
  // anyway: a single malformed event must never white-screen the dashboard.
  const attempts = l.attempts ?? []
  return (
    <tr key={l.id} className="border-t border-white/5 hover:bg-white/[0.03]"
      title={attempts.map((a) => `${a.alias}#${a.account_id} → ${a.status}${a.err ? ' ' + a.err : ''}`).join('  |  ')}>
      <td className="px-3 py-1.5 whitespace-nowrap text-gray-400">{fmtTime(l.ts)}</td>
      <td className="px-3 py-1.5 whitespace-nowrap">
        <span className={l.proto === 'anthropic' ? 'text-fuchsia-300' : 'text-cyan-300'}>{l.proto === 'anthropic' ? 'A' : 'O'}</span>
      </td>
      <td className="px-3 py-1.5 whitespace-nowrap text-gray-300 max-w-[10rem] truncate">{l.model || '-'}</td>
      <td className="px-3 py-1.5 whitespace-nowrap">
        {l.cold ? <span className="text-sky-300">冷</span> : l.warm ? <span className="text-orange-300">暖</span> : <span className="text-gray-600">-</span>}
      </td>
      <td className="px-3 py-1.5 whitespace-nowrap">
        <span className="inline-flex items-center gap-1">
          {attempts.map((a, i) => (
            <span key={i} className="inline-flex items-center gap-1">
              {i > 0 && <span className="text-gray-600">→</span>}
              <span className={`px-1.5 py-0.5 rounded border ${attemptStyle(a.status)}`}>
                {(a.alias || `#${a.account_id}`).slice(0, 10)}·{a.status || 'net'}
              </span>
            </span>
          ))}
          {attempts.length === 0 && <span className="text-gray-600">-</span>}
        </span>
      </td>
      <td className="px-3 py-1.5 whitespace-nowrap text-gray-400">{l.retries || ''}</td>
      <td className="px-3 py-1.5 whitespace-nowrap text-gray-400">{l.ttft_ms != null ? `${l.ttft_ms}ms` : '-'}</td>
      <td className="px-3 py-1.5 whitespace-nowrap text-gray-300">
        {(l.prompt_tokens || l.completion_tokens)
          ? <span className="text-gray-400">{l.prompt_tokens ?? 0}<span className="text-gray-600"> → </span>{l.completion_tokens ?? 0}</span>
          : '-'}
      </td>
      <td className="px-3 py-1.5 whitespace-nowrap">
        {l.cached_tokens ? <span className="text-emerald-300">{l.cached_tokens}</span> : <span className="text-gray-600">-</span>}
      </td>
      <td className="px-3 py-1.5 whitespace-nowrap text-gray-400">{l.dur_ms}ms</td>
      <td className={`px-3 py-1.5 whitespace-nowrap font-medium ${outcomeStyle(l.outcome)}`}>{outcomeLabel(l.outcome)}</td>
    </tr>
  )
})

export function RequestLogStream({ authed }: { authed: boolean }) {
  // Live request-log feed state lives HERE, not in App, so a new SSE frame
  // (setLogs) re-renders only this component — not TopBar, the charts, or the
  // account grid. App passes just `authed` so the SSE effect can keep its
  // if (!authed) return early-out (no stream before login).
  const [logs, setLogs] = useState<RequestLog[]>([])
  const [paused, setPaused] = useState(false)
  const [autoPaused, setAutoPaused] = useState(false)
  const [mode, setMode] = useState<'live' | 'errors'>('live')
  const [loadingErrors, setLoadingErrors] = useState(false)
  const [conn, setConn] = useState<StreamState>({ connected: false })
  const [loadingEarlier, setLoadingEarlier] = useState(false)
  const [hasEarlier, setHasEarlier] = useState(true)
  // pausedRef lets the SSE callback read the current pause state without
  // re-subscribing the stream on every toggle (the effect dep is [authed]).
  const pausedRef = useRef(false)
  useEffect(() => { pausedRef.current = paused }, [paused])
  // modeRef mirrors `mode` so the SSE callback can gate on it without the
  // effect re-subscribing (dep is [authed]). In 'errors' mode live frames are
  // dropped so the all-errors list isn't interleaved with new traffic.
  const modeRef = useRef<'live' | 'errors'>('live')
  useEffect(() => { modeRef.current = mode }, [mode])

  // Auto-pause after AUTO_PAUSE_MS of being live. (Re)arms whenever the feed is
  // running (authed && !paused && live mode) and is cleared otherwise, so a
  // manual resume always starts a fresh window. Sets `autoPaused` so the label
  // can tell the user it stopped on its own (vs. a manual pause). Skipped in
  // 'errors' mode — the live feed is already displaced there.
  useEffect(() => {
    if (!authed || paused || mode !== 'live') return
    const t = setTimeout(() => {
      setPaused(true)
      setAutoPaused(true)
    }, AUTO_PAUSE_MS)
    return () => clearTimeout(t)
  }, [authed, paused, mode])
  // logsRef lets loadEarlier read the current oldest ts without depending on
  // `logs` (which would recreate the callback on every SSE frame).
  const logsRef = useRef<RequestLog[]>([])
  useEffect(() => { logsRef.current = logs }, [logs])

  // Batch incoming SSE events. On connect the feed replays a capped slice of
  // the ring (we ask for the newest 100 — the ring can be up to 1000, but
  // older entries are paginated via "加载更早") in one synchronous burst;
  // calling setLogs per event was O(n²) — each did a full slice + unshift and
  // triggered a re-render of every row. Accumulate into pendingRef and flush
  // once per microtask: one render per burst, and the merge is a single
  // O(batch+prev) prepend.
  const pendingRef = useRef<RequestLog[]>([])
  const flushScheduledRef = useRef(false)
  const flush = useCallback(() => {
    flushScheduledRef.current = false
    const batch = pendingRef.current
    if (batch.length === 0) return
    pendingRef.current = []
    setLogs((prev) => {
      // Events arrive oldest→newest; reverse so index 0 stays the newest, then
      // prepend to the existing newest-first list. Cap in-memory at 2000.
      const merged = batch.length > 1 ? batch.slice().reverse().concat(prev) : [batch[0], ...prev]
      return merged.length > 2000 ? merged.slice(0, 2000) : merged
    })
  }, [])

  useEffect(() => {
    if (!authed) return
    const stop = streamEvents(
      (log) => {
        if (pausedRef.current || modeRef.current !== 'live') return // freeze while paused / viewing all-errors
        pendingRef.current.push(log)
        if (!flushScheduledRef.current) {
          flushScheduledRef.current = true
          queueMicrotask(flush)
        }
      },
      setConn,
      100,
    )
    return stop
  }, [authed])

  const loadEarlier = async () => {
    setLoadingEarlier(true)
    try {
      const oldest = logsRef.current.length ? logsRef.current[logsRef.current.length - 1].ts : Date.now()
      const older = await fetchLogs({ before: oldest, limit: 100 })
      if (older.length === 0) {
        setHasEarlier(false)
        return
      }
      // DB returns newest-first; logs is newest-first too, so append the page
      // (it's all older than our current oldest) as-is to the tail.
      setLogs((prev) => [...prev, ...older])
      if (older.length < 100) setHasEarlier(false)
    } finally {
      setLoadingEarlier(false)
    }
  }

  // "全部错误": errors are rare, so fetch the full set (up to the store's
  // 2000-row ceiling) from /admin/logs?outcome=error in one shot instead of
  // paging through all outcomes. The live SSE feed is gated while the error
  // list is displayed so new traffic doesn't interleave; "返回实时" repopulates
  // recent history and hands the view back to the live stream.
  const loadAllErrors = async () => {
    modeRef.current = 'errors' // gate SSE before the await so live frames can't mix in
    setMode('errors')
    setOutcome('') // the list is already all-errors; clear any prior client filter
    setLoadingErrors(true)
    try {
      const errs = await fetchLogs({ outcome: 'error', limit: 2000 })
      setLogs(errs)
      setHasEarlier(false)
    } finally {
      setLoadingErrors(false)
    }
  }

  const backToLive = () => {
    modeRef.current = 'live'
    setMode('live')
    setHasEarlier(true)
    // The SSE ring replay was dropped while gated, so pull the newest 100 from
    // the store rather than showing an empty table until the next live event.
    fetchLogs({ limit: 100 }).then((r) => setLogs(r)).catch(() => {})
  }

  const onClear = () => { setLogs([]); setHasEarlier(true) }

  const [outcome, setOutcome] = useState('')
  const [proto, setProto] = useState('')
  const [q, setQ] = useState('')
  const [stick, setStick] = useState(true)
  const scrollRef = useRef<HTMLDivElement>(null)

  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return logs.filter((l) => {
      if (outcome && !matchesOutcome(l, outcome)) return false
      if (proto && l.proto !== proto) return false
      if (needle && !`${l.model} ${l.final_alias} ${l.key_hash}`.toLowerCase().includes(needle)) return false
      return true
    })
  }, [logs, outcome, proto, q])

  // Newest-first list (index 0 = most recent). Auto-scroll stays pinned to the
  // TOP while sticky; stop following when the user scrolls down to inspect
  // something, like a terminal (new rows appear at the top, so the "newest
  // edge" is the top).
  useEffect(() => {
    if (stick && scrollRef.current) {
      scrollRef.current.scrollTop = 0
    }
  }, [filtered.length, stick])

  const onScroll = () => {
    const el = scrollRef.current
    if (!el) return
    setStick(el.scrollTop < 40)
  }

  return (
    <section className="mb-6">
      <div className="flex items-center justify-between mb-3 gap-2 flex-wrap">
        <div className="flex items-center gap-2">
          <h2 className="text-sm font-semibold text-gray-300">{mode === 'errors' ? '全部错误日志' : '实时请求流'}</h2>
          {mode === 'live' && (
            <span className={`flex items-center gap-1 text-xs ${conn.connected ? 'text-emerald-400' : 'text-amber-400'}`}>
              {conn.connected ? <Wifi className="w-3.5 h-3.5" /> : <WifiOff className="w-3.5 h-3.5" />}
              {conn.connected ? '已连接' : '重连中'}
            </span>
          )}
          {mode === 'live' && paused && <span className="text-xs text-gray-500">{autoPaused ? '（已自动暂停）' : '（已暂停）'}</span>}
          {mode === 'errors' && <span className="text-xs text-amber-300">一次性全量查询</span>}
          <span className="text-xs text-gray-600">{filtered.length} 条</span>
        </div>
        <div className="flex items-center gap-2">
          <select value={proto} onChange={(e) => setProto(e.target.value)}
            className="bg-white/5 border border-white/10 rounded-md px-2 py-1 text-xs text-gray-300">
            <option value="">全部协议</option>
            <option value="openai">OpenAI</option>
            <option value="anthropic">Anthropic</option>
          </select>
          <select value={outcome} onChange={(e) => setOutcome(e.target.value)} disabled={mode === 'errors'}
            className="bg-white/5 border border-white/10 rounded-md px-2 py-1 text-xs text-gray-300 disabled:opacity-50">
            <option value="">全部结果</option>
            <option value="ok">成功</option>
            <option value="error">错误</option>
            <option value="throttled">429</option>
          </select>
          <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="模型/账号/会话…"
            className="bg-white/5 border border-white/10 rounded-md px-2 py-1 text-xs text-gray-300 w-40" />
          {mode === 'errors' ? (
            <button onClick={backToLive}
              className="flex items-center gap-1 px-2 py-1 rounded-md text-xs bg-amber-500/15 hover:bg-amber-500/25 text-amber-300 border border-amber-500/30">
              <Play className="w-3.5 h-3.5" />返回实时
            </button>
          ) : (
            <button onClick={loadAllErrors} disabled={loadingErrors}
              className="flex items-center gap-1 px-2 py-1 rounded-md text-xs bg-white/5 hover:bg-white/10 text-gray-300 disabled:opacity-50">
              <AlertTriangle className="w-3.5 h-3.5" />{loadingErrors ? '加载中…' : '全部错误'}
            </button>
          )}
          {mode === 'live' && (
            <button onClick={() => { setPaused(v => !v); setAutoPaused(false) }}
              className="flex items-center gap-1 px-2 py-1 rounded-md text-xs bg-white/5 hover:bg-white/10 text-gray-300">
              {paused ? <Play className="w-3.5 h-3.5" /> : <Pause className="w-3.5 h-3.5" />}
              {paused ? '继续' : '暂停'}
            </button>
          )}
          <button onClick={onClear}
            className="flex items-center gap-1 px-2 py-1 rounded-md text-xs bg-white/5 hover:bg-white/10 text-gray-300">
            <Trash2 className="w-3.5 h-3.5" />清空
          </button>
        </div>
      </div>

      <div className="rounded-2xl border border-white/10 bg-white/[0.02] overflow-hidden">
        {hasEarlier && mode === 'live' && (
          <div className="border-b border-white/5 p-2 text-center">
            <button onClick={loadEarlier} disabled={loadingEarlier}
              className="inline-flex items-center gap-1 px-3 py-1 rounded-md text-xs bg-white/5 hover:bg-white/10 text-gray-300 disabled:opacity-50">
              <History className="w-3.5 h-3.5" />{loadingEarlier ? '加载中…' : '加载更早'}
            </button>
          </div>
        )}
        <div ref={scrollRef} onScroll={onScroll}
          className="overflow-auto max-h-[28rem] font-mono text-xs">
          <table className="w-full">
            <thead className="sticky top-0 bg-ink-900/95 backdrop-blur text-gray-500">
              <tr>
                {['时间', '协议', '模型', '温度', '调度轨迹', '重试', 'TTFT', 'token (入→出)', 'cache', '耗时', '结果'].map((h) => (
                  <th key={h} className="text-left font-medium px-3 py-2 whitespace-nowrap">{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {filtered.length === 0 && (
                <tr><td colSpan={11} className="px-3 py-10 text-center text-gray-600">暂无请求记录</td></tr>
              )}
              {filtered.map((l) => (
                <Row key={l.id} l={l} />
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </section>
  )
}

function matchesOutcome(l: RequestLog, sel: string): boolean {
  if (sel === 'ok') return l.outcome === 'ok'
  if (sel === 'throttled') return l.outcome === 'throttled' || (l.attempts ?? []).some((a) => a.status === 429)
  if (sel === 'error') return ['no_accounts', 'server_error', 'client_error'].includes(l.outcome)
  return true
}

function outcomeLabel(o: string): string {
  switch (o) {
    case 'ok': return '成功'
    case 'no_accounts': return '无可用账号'
    case 'server_error': return '上游错误'
    case 'client_error': return '请求错误'
    case 'throttled': return '限流'
    default: return o
  }
}
