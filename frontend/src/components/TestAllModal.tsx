import { Loader2, CheckCircle2, XCircle } from 'lucide-react'
import { useState, useEffect } from 'react'
import { apiFetch } from '../api'
import { GlassModal } from './ui/GlassModal'

type R = { id: number; alias: string; ok: boolean; status: number; error: string }

export function TestAllModal({ onClose, onUpdate }: { onClose: () => void; onUpdate: () => Promise<void> | (() => void) }) {
  const [loading, setLoading] = useState(true)
  const [results, setResults] = useState<R[]>([])

  const run = async () => {
    setLoading(true)
    setResults([])
    try {
      const r = await apiFetch('/admin/test-all', { method: 'POST' })
      const rs: R[] = await r.json()
      setResults(rs)
      await Promise.all(rs.map(rr => apiFetch(`/admin/accounts/${rr.id}/refresh`, { method: 'POST' }).catch(() => {})))
      await onUpdate()
    } catch { /* ignore */ } finally { setLoading(false) }
  }
  useEffect(() => { run() }, [])

  const okCount = results.filter(r => r.ok).length

  return (
    <GlassModal onClose={onClose} title="普测所有账号" wide>
      {loading ? (
        <div className="flex items-center justify-center gap-2 text-gray-400 text-sm py-8">
          <Loader2 className="w-4 h-4 animate-spin" />正在逐个测试…(并发)
        </div>
      ) : (
        <>
          <div className="text-sm text-gray-300 mb-3">汇总:<span className="text-emerald-400 font-semibold"> {okCount} </span>/ {results.length} 可用</div>
          <div className="flex flex-col gap-1.5 max-h-80 overflow-y-auto">
            {results.map(r => (
              <div key={r.id} className="flex items-center gap-2 bg-white/[0.04] rounded-lg px-3 py-2 text-sm">
                {r.ok ? <CheckCircle2 className="w-4 h-4 text-emerald-400 shrink-0" /> : <XCircle className="w-4 h-4 text-rose-400 shrink-0" />}
                <span className="truncate flex-1">{r.alias}</span>
                <span className={`text-xs ${r.ok ? 'text-emerald-400' : 'text-rose-400'}`}>{r.ok ? '可用' : `失败${r.status ? ` ${r.status}` : ''}`}</span>
              </div>
            ))}
          </div>
          <button onClick={run} className="mt-4 flex items-center justify-center gap-2 bg-white/5 hover:bg-white/10 rounded-lg py-2 text-sm font-medium transition-colors">重测</button>
        </>
      )}
    </GlassModal>
  )
}
