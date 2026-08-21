export function CooldownRing({ secs, total = 60, size = 28 }: { secs: number; total?: number; size?: number }) {
  const r = (size - 4) / 2
  const c = 2 * Math.PI * r
  const off = c * (1 - Math.min(secs / total, 1))
  return (
    <svg width={size} height={size} className="-rotate-90 shrink-0">
      <circle cx={size / 2} cy={size / 2} r={r} fill="none" stroke="rgba(255,255,255,.12)" strokeWidth="2" />
      <circle cx={size / 2} cy={size / 2} r={r} fill="none" stroke="#f59e0b" strokeWidth="2"
        strokeDasharray={c} strokeDashoffset={off} strokeLinecap="round" />
      <text x="50%" y="50%" dy="0.35em" textAnchor="middle" className="rotate-90" fontSize="9" fill="#fcd34d">{secs}</text>
    </svg>
  )
}
