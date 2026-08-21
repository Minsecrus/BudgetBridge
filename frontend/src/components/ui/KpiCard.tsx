import { motion } from 'framer-motion'
import { AnimatedNumber } from './AnimatedNumber'
import { Sparkline } from './Sparkline'
import { Skeleton } from './Skeleton'

export function KpiCard({
  label, value, decimals = 0, prefix = '', suffix = '',
  spark = [], color = '#67e8f9', loading,
}: {
  label: string
  value: number
  decimals?: number
  prefix?: string
  suffix?: string
  spark?: number[]
  color?: string
  loading?: boolean
}) {
  return (
    <motion.div
      initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }}
      whileHover={{ y: -3 }}
      className="glass rounded-2xl p-4 shadow-glass">
      <div className="text-[11px] uppercase tracking-wide text-gray-400">{label}</div>
      {loading
        ? <Skeleton className="h-8 w-24 mt-2" />
        : <AnimatedNumber value={value} decimals={decimals} prefix={prefix} suffix={suffix}
            className="block text-2xl font-bold text-gradient mt-1" />}
      <div className="mt-2 -mx-1">{spark.length > 0 && <Sparkline data={spark} color={color} />}</div>
    </motion.div>
  )
}
