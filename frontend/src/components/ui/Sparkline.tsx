import { Line, LineChart, ResponsiveContainer } from 'recharts'

export function Sparkline({ data, color = '#67e8f9' }: { data: number[]; color?: string }) {
  if (data.length === 0) return null
  const d = data.map((v, i) => ({ i, v }))
  return (
    <ResponsiveContainer width="100%" height={32}>
      <LineChart data={d}>
        <Line type="monotone" dataKey="v" stroke={color} strokeWidth={2} dot={false} isAnimationActive />
      </LineChart>
    </ResponsiveContainer>
  )
}
