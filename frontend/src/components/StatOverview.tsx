import { memo } from 'react'
import { KpiCard } from './ui/KpiCard'
import { Skeleton } from './ui/Skeleton'
import type { StatsResponse, Window } from '../types'

export const StatOverview = memo(function StatOverview({
  stats, loading, window: w, onWindow,
}: {
  stats: StatsResponse | null
  loading: boolean
  window: Window
  onWindow: (w: Window) => void
}) {
  const g = stats?.global
  const spark = (stats?.timeline ?? []).map(p => p.req)
  return (
    <section className="mb-6">
      <div className="flex items-center justify-between mb-3">
        <h2 className="text-sm font-semibold text-gray-300">概览</h2>
        <div className="flex gap-1 bg-white/5 rounded-lg p-0.5">
          {(['1h', '24h', '7d'] as Window[]).map(opt => (
            <button key={opt} onClick={() => onWindow(opt)}
              className={`px-2.5 py-1 rounded-md text-xs transition-colors ${w === opt ? 'bg-white/15 text-white' : 'text-gray-400 hover:text-white'}`}>
              {opt}
            </button>
          ))}
        </div>
      </div>
      {loading && !stats ? (
        <div className="grid grid-cols-2 md:grid-cols-6 gap-3">
          {Array.from({ length: 6 }).map((_, i) => <Skeleton key={i} className="h-24 rounded-2xl" />)}
        </div>
      ) : (
        <div className="grid grid-cols-2 md:grid-cols-6 gap-3">
          <KpiCard label="总余额" value={g?.total_balance ?? 0} decimals={2} prefix="¥" loading={loading}
            color="#a78bfa" spark={(stats?.per_account ?? []).map(a => a.balance)} />
          <KpiCard label="可用率" value={g && g.total ? (g.available / g.total) * 100 : 0} decimals={0} suffix="%"
            color="#34d399" loading={loading} />
          <KpiCard label="累计请求" value={g?.requests_total ?? 0} color="#67e8f9" loading={loading} spark={spark} />
          <KpiCard label={`${w} 请求`} value={g?.requests_window ?? 0} color="#f0abfc" loading={loading} spark={spark} />
          <KpiCard label="成功率" value={(g?.success_rate ?? 0) * 100} decimals={1} suffix="%"
            color="#34d399" loading={loading} />
          <KpiCard label={`${w} 网络重试`} value={g?.network_retries_window ?? 0} color="#fbbf24" loading={loading} spark={(stats?.timeline ?? []).map(p => p.net_retry)} />
        </div>
      )}
    </section>
  )
})
