import { memo } from 'react'
import { Bar, CartesianGrid, ComposedChart, Line, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import type { StatsResponse, Window } from '../types'
import { buildTrendRows, trendTickLabel } from './trendData'

export const TrendChart = memo(function TrendChart({ stats, window }: { stats: StatsResponse | null; window: Window }) {
  const data = buildTrendRows(stats?.timeline ?? [], window)
  return (
    <div className="glass rounded-2xl p-4 shadow-glass">
      <h3 className="text-sm font-semibold text-gray-300 mb-3">请求量 / 成功率</h3>
      <ResponsiveContainer width="100%" height={220}>
        <ComposedChart data={data} margin={{ top: 4, right: 8, bottom: 0, left: -18 }}>
          <CartesianGrid stroke="rgba(255,255,255,.06)" vertical={false} />
          {/* dataKey "t" is the full unique per-point key (date+time for 7d); tickFormatter
              only trims what's printed under the axis (date for 7d) to avoid overlap. */}
          <XAxis dataKey="t" tick={{ fill: '#9ca3af', fontSize: 10 }}
            minTickGap={window === '7d' ? 48 : 24}
            tickFormatter={(v: string) => trendTickLabel(v, window)} />
          <YAxis yAxisId="l" tick={{ fill: '#9ca3af', fontSize: 10 }} allowDecimals={false} />
          <YAxis yAxisId="r" orientation="right" domain={[0, 100]} tick={{ fill: '#9ca3af', fontSize: 10 }} />
          <Tooltip contentStyle={{ background: '#15151f', border: '1px solid rgba(255,255,255,.12)', borderRadius: 12, fontSize: 12 }}
            formatter={(value, name) => {
              if (name === '成功率') {
                return [value == null ? '—' : `${Math.round(Number(value))}%`, '成功率']
              }
              return [value == null ? '0' : `${value}`, '请求量']
            }} />
          <Bar yAxisId="l" dataKey="req" name="请求量" fill="#7c3aed" radius={[3, 3, 0, 0]} isAnimationActive={false} />
          <Line yAxisId="r" type="monotone" dataKey="rate" name="成功率" stroke="#34d399"
            strokeWidth={2} dot={false} isAnimationActive={false}
            // null gaps (req=0 buckets) break the line rather than dipping to 0
            connectNulls={false} />
        </ComposedChart>
      </ResponsiveContainer>
    </div>
  )
})
