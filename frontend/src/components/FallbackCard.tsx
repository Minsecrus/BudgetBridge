import { memo, useState } from 'react'
import { motion } from 'framer-motion'
import { Power, Trash2, Pencil, CheckCircle2, XCircle, FlaskConical } from 'lucide-react'
import type { FallbackChannel } from '../types'
import { toggleFallback, deleteFallback, testFallback } from '../api'
import { toast } from './Toast'

// FallbackCard mirrors AccountCard's skeleton (motion enter, busy-keyed action
// buttons, 2-click delete) but drops everything balance/cooldown-specific:
// no ¥ total, no coupon count, no cooldown ring, no "查余额/解冻". A fallback
// channel only has a name, an endpoint, a model whitelist, and an enabled flag.
export const FallbackCard = memo(function FallbackCard({ channel, onUpdate, onEdit, index = 0 }: {
  channel: FallbackChannel
  onUpdate: () => Promise<void>
  onEdit: (channel: FallbackChannel) => void
  index?: number
}) {
  const [busy, setBusy] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const disabled = !channel.enabled

  const handleToggle = async () => {
    setBusy('toggle')
    try {
      const ok = await toggleFallback(channel.id)
      toast(ok ? (channel.enabled ? '已停用' : '已启用') : '操作失败', ok ? 'success' : 'error')
      if (ok) await onUpdate()
    } finally {
      setBusy(null)
    }
  }

  // Probe this channel's endpoint with its own key (bypasses the pool). Fails
  // return a non-200 status; surface it in the toast like AccountCard does.
  const handleTest = async () => {
    setBusy('test')
    try {
      const b = await testFallback(channel.id)
      if (b.ok) toast('单测可用', 'success')
      else toast(`单测失败${b.status ? ` ${b.status}` : ''}${b.error ? ` ${b.error}` : ''}`, 'error')
    } finally {
      setBusy(null)
    }
  }

  const handleDelete = async () => {
    if (!confirmDelete) { setConfirmDelete(true); setTimeout(() => setConfirmDelete(false), 3000); return }
    setBusy('delete')
    try {
      const ok = await deleteFallback(channel.id)
      toast('已删除通道', ok ? 'success' : 'error')
      if (ok) await onUpdate()
    } finally {
      setBusy(null)
      setConfirmDelete(false)
    }
  }

  return (
    <motion.div
      initial={{ opacity: 0, y: 14 }} animate={{ opacity: 1, y: 0 }}
      transition={{ delay: Math.min(index * 0.04, 0.4) }}
      whileHover={{ y: -4 }}
      className={`rounded-2xl p-5 flex flex-col gap-4 transition-colors ${disabled ? 'bg-ink-800/60 border border-white/[0.06] opacity-75' : 'glass shadow-glass'}`}>
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2 min-w-0">
          {disabled
            ? <XCircle className="w-4 h-4 text-gray-500 shrink-0" />
            : <CheckCircle2 className="w-4 h-4 text-emerald-400 animate-pulse-glow shrink-0" />}
          <span className={`font-semibold text-sm truncate ${disabled ? 'text-gray-500' : 'text-gray-100'}`}>{channel.name || '未命名'}</span>
        </div>
        {disabled
          ? <span className={`text-[10px] px-2 py-0.5 rounded-full border shrink-0 ${channel.disabled_by_err ? 'bg-rose-500/15 text-rose-300 border-rose-500/30' : 'bg-slate-700/40 text-slate-400 border-white/10'}`}>{channel.disabled_by_err ? '异常禁用' : '已停用'}</span>
          : <span className="text-xs text-gray-400 shrink-0">#{channel.id}</span>}
      </div>

      <div className="text-xs text-gray-400 font-mono truncate" title={channel.base_url}>{channel.base_url}</div>

      <div className="flex flex-wrap gap-1.5 min-h-[1.25rem]">
        {channel.models.length === 0 ? (
          <span className="text-xs text-gray-500">未配置模型（不匹配任何请求）</span>
        ) : channel.models.map(m => (
          <span key={m} className="text-[11px] px-2 py-0.5 rounded-full bg-violet-500/10 text-violet-200 border border-violet-500/20">{m}</span>
        ))}
      </div>

      <div className="flex items-center gap-1 pt-1 border-t border-white/10">
        <button onClick={handleToggle} disabled={!!busy} title={channel.enabled ? '停用' : '启用'}
          className={`p-2 rounded-lg transition-colors disabled:opacity-40 ${channel.enabled ? 'text-rose-300 hover:bg-rose-500/10' : 'text-emerald-300 hover:bg-emerald-500/10'}`}>
          <Power className="w-4 h-4" />
        </button>
        <button onClick={handleTest} disabled={!!busy} title="单测"
          className="p-2 rounded-lg text-gray-400 hover:text-white hover:bg-white/10 transition-colors disabled:opacity-40">
          <FlaskConical className={`w-4 h-4 ${busy === 'test' ? 'animate-spin' : ''}`} />
        </button>
        <button onClick={() => onEdit(channel)} disabled={!!busy} title="编辑"
          className="p-2 rounded-lg text-gray-400 hover:text-white hover:bg-white/10 transition-colors disabled:opacity-40">
          <Pencil className="w-4 h-4" />
        </button>
        <button onClick={handleDelete} disabled={!!busy} title={confirmDelete ? '再次点击确认删除' : '删除'}
          className={`p-2 rounded-lg transition-colors disabled:opacity-40 ml-auto ${confirmDelete ? 'bg-rose-600 text-white hover:bg-rose-500' : 'text-rose-300 hover:bg-rose-500/10'}`}>
          <Trash2 className="w-4 h-4" />
        </button>
      </div>
    </motion.div>
  )
})
