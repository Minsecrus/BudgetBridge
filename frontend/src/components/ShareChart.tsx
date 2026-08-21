import { memo } from 'react'
import { Cell, Pie, PieChart, ResponsiveContainer, Tooltip } from 'recharts'
import type { StatsResponse } from '../types'

const COLORS = ['#a78bfa', '#67e8f9', '#f0abfc', '#34d399', '#fbbf24', '#fb7185']

export const ShareChart = memo(function ShareChart({ stats }: { stats: StatsResponse | null }) {
  const data = (stats?.per_account ?? [])
    .filter(a => a.requests_window > 0)
    .map(a => ({ name: a.alias, value: a.requests_window }))
  return (
    <div className="glass rounded-2xl p-4 shadow-glass">
      <h3 className="text-sm font-semibold text-gray-300 mb-3">各账号请求占比</h3>
      {data.length === 0 ? (
        <div className="h-[220px] flex items-center justify-center text-gray-500 text-sm">窗口内暂无请求</div>
      ) : (
        <ResponsiveContainer width="100%" height={220}>
          <PieChart>
            <Pie data={data} dataKey="value" nameKey="name" innerRadius={50} outerRadius={80} paddingAngle={2} isAnimationActive={false}>
              {data.map((_, i) => <Cell key={i} fill={COLORS[i % COLORS.length]} />)}
            </Pie>
            <Tooltip contentStyle={{ background: '#15151f', border: '1px solid rgba(255,255,255,.12)', borderRadius: 12, fontSize: 12 }} />
          </PieChart>
        </ResponsiveContainer>
      )}
    </div>
  )
})
