import { useEffect, useState, useCallback } from 'react'
import { AnimatePresence } from 'framer-motion'
import { TopBar } from './components/TopBar'
import { AccountCard } from './components/AccountCard'
import { FallbackCard } from './components/FallbackCard'
import { FallbackModal } from './components/FallbackModal'
import { LoginPage } from './components/LoginPage'
import { ToastContainer } from './components/Toast'
import { StatOverview } from './components/StatOverview'
import { TrendChart } from './components/TrendChart'
import { ShareChart } from './components/ShareChart'
import { RequestLogStream } from './components/RequestLogStream'
import { apiFetch, clearToken, fetchStats, listFallbacks } from './api'
import type { AccountStatus, FallbackChannel, StatsResponse, Window } from './types'

export default function App() {
  const [accounts, setAccounts] = useState<AccountStatus[]>([])
  const [stats, setStats] = useState<StatsResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [compact, setCompact] = useState(false)
  const [authed, setAuthed] = useState(true)
  const [win, setWin] = useState<Window>('24h')
  const [fallbacks, setFallbacks] = useState<FallbackChannel[]>([])
  const [showAddFallback, setShowAddFallback] = useState(false)
  const [editTarget, setEditTarget] = useState<FallbackChannel | null>(null)

  // Any 401 from apiFetch fires this event → show login
  useEffect(() => {
    const handler = () => setAuthed(false)
    window.addEventListener('bb:unauthorized', handler)
    return () => window.removeEventListener('bb:unauthorized', handler)
  }, [])

  const fetchAccounts = useCallback(async () => {
    try {
      const res = await apiFetch('/admin/accounts')
      if (res.status === 401) { clearToken(); setAuthed(false); return }
      const next = (await res.json()) as AccountStatus[]
      // Poll returns a fresh object every time, but identical data must not
      // re-render the whole tree. Returning the SAME ref from the updater makes
      // React bail out (no re-render); only a real change produces a new ref.
      setAccounts(prev => (JSON.stringify(prev) === JSON.stringify(next) ? prev : next))
    } finally {
      setLoading(false)
    }
  }, [])

  const refreshStats = useCallback(async () => {
    const next = await fetchStats(win)
    if (!next) return // fetch failed — keep the last stats
    setStats(prev => (prev && JSON.stringify(prev) === JSON.stringify(next) ? prev : next))
  }, [win])

  // Fallback channels poll separately (same 10s cadence + ref-equality bail-out
  // as accounts). 401 is surfaced centrally by apiFetch via bb:unauthorized.
  const fetchFallbacks = useCallback(async () => {
    const next = await listFallbacks()
    if (!next) return
    setFallbacks(prev => (JSON.stringify(prev) === JSON.stringify(next) ? prev : next))
  }, [])

  useEffect(() => {
    fetchAccounts()
    const t = setInterval(fetchAccounts, 10_000)
    return () => clearInterval(t)
  }, [fetchAccounts])

  useEffect(() => {
    refreshStats()
    const t = setInterval(refreshStats, 15_000)
    return () => clearInterval(t)
  }, [refreshStats])

  useEffect(() => {
    fetchFallbacks()
    const t = setInterval(fetchFallbacks, 10_000)
    return () => clearInterval(t)
  }, [fetchFallbacks])

  if (!authed) return <LoginPage onLogin={() => { setAuthed(true); fetchAccounts(); refreshStats(); fetchFallbacks() }} />

  return (
    <div className="min-h-screen bg-ink-950 text-gray-100 relative overflow-hidden">
      <div className="pointer-events-none fixed inset-0 bg-aurora animate-aurora-float opacity-70" />
      <div className="relative">
        <TopBar accounts={accounts} onUpdate={fetchAccounts} compact={compact} onToggleCompact={() => setCompact(v => !v)} />
        <main className="max-w-7xl mx-auto px-6 py-8">
          <StatOverview stats={stats} loading={loading} window={win} onWindow={setWin} />
          <div className="grid grid-cols-1 lg:grid-cols-3 gap-4 mb-8">
            <div className="lg:col-span-2"><TrendChart stats={stats} window={win} /></div>
            <ShareChart stats={stats} />
          </div>
          <h2 className="text-sm font-semibold text-gray-300 mb-3">账号池</h2>
          {loading ? null : accounts.length === 0 ? (
            <div className="text-center py-32 text-gray-400">暂无账号</div>
          ) : compact ? (
            <div className="flex flex-col gap-1">
              {accounts.map(acc => (
                <AccountCard key={acc.id} account={acc} onUpdate={fetchAccounts} compact />
              ))}
            </div>
          ) : (
            <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-4">
              {accounts.map((acc, i) => (
                <AccountCard key={acc.id} account={acc} onUpdate={fetchAccounts} index={i} />
              ))}
            </div>
          )}
          <div className="flex items-center justify-between mb-3 mt-10">
            <h2 className="text-sm font-semibold text-gray-300">兜底通道</h2>
            <button onClick={() => setShowAddFallback(true)}
              className="px-3 py-1.5 rounded-lg text-xs font-medium bg-violet-600 hover:bg-violet-500 transition-colors">
              + 添加通道
            </button>
          </div>
          {fallbacks.length === 0 ? (
            <div className="text-center py-12 text-gray-400 text-sm">暂无兜底通道（号池耗尽时无备份）</div>
          ) : (
            <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
              {fallbacks.map((fb, i) => (
                <FallbackCard key={fb.id} channel={fb} onUpdate={fetchFallbacks} onEdit={setEditTarget} index={i} />
              ))}
            </div>
          )}
          <div className="mt-8">
            <RequestLogStream authed={authed} />
          </div>
        </main>
      </div>
      <AnimatePresence>
        {showAddFallback && (
          <FallbackModal mode="add" onClose={() => setShowAddFallback(false)} onSaved={fetchFallbacks} />
        )}
        {editTarget && (
          <FallbackModal mode="edit" channel={editTarget} onClose={() => setEditTarget(null)} onSaved={fetchFallbacks} />
        )}
      </AnimatePresence>
      <ToastContainer />
    </div>
  )
}
