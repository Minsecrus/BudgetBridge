import { useEffect, useRef } from 'react'
import { animate, useInView } from 'framer-motion'

export function AnimatedNumber({
  value, decimals = 0, prefix = '', suffix = '', className,
}: {
  value: number
  decimals?: number
  prefix?: string
  suffix?: string
  className?: string
}) {
  const ref = useRef<HTMLSpanElement>(null)
  const prev = useRef(0)
  const inView = useInView(ref, { once: false, amount: 0.5 })

  useEffect(() => {
    const el = ref.current
    if (!el) return
    const controls = animate(prev.current, value, {
      duration: 0.6,
      ease: 'easeOut',
      onUpdate: v => { el.textContent = prefix + v.toFixed(decimals) + suffix },
    })
    prev.current = value
    return () => controls.stop()
  }, [value, decimals, prefix, suffix, inView])

  return <span ref={ref} className={className}>{prefix}{(0).toFixed(decimals)}{suffix}</span>
}
