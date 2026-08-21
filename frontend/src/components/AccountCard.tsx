import { memo, useState } from 'react'
import { motion } from 'framer-motion'
import { RefreshCw, Power, Timer, Trash2, CheckCircle2, XCircle, Clock, TrendingUp, FlaskConical } from 'lucide-react'
import { AnimatedNumber } from './ui/AnimatedNumber'
import { CooldownRing } from './ui/CooldownRing'
import type { AccountStatus } from '../types'
import { apiFetch } from '../api'
import { toast } from './Toast'

function StatusDot({ a }: { a: AccountStatus }) {
  if (!a.enabled) return <XCircle className="w-4 h-4 text-gray-500" />
  if (a.cooldown_secs > 0) return <Clock className="w-4 h-4 text-amber-400 animate-pulse-glow" />
  return <CheckCircle2 className="w-4 h-4 text-emerald-400 animate-pulse-glow" />
}

function balanceColor(b: number) {
  if (b >= 10) return 'text-emerald-300'
  if (b >= 3) return 'text-amber-300'
  return 'text-rose-300'
}

export const AccountCard = memo(function AccountCard({ account, onUpdate, compact = false, index = 0 }: {
  account: AccountStatus
  onUpdate: () => Promise<void>
  compact?: boolean
  index?: number
}) {
  const [busy, setBusy] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)

  const call = async (path: string, key: string, okMsg?: string) => {
    setBusy(key)
    try {
      const r = await apiFetch(`/admin/accounts/${account.id}/${path}`, { method: 'POST' })
      if (okMsg) {
        // The refresh endpoint returns 200 with {ok:false,error} on balance-check
        // failure (so the UI can show the real cause), so consult the body's
        // ok flag rather than the HTTP status.
        const body = await r.json().catch(() => ({} as { ok?: boolean; error?: string }))
        if (r.ok && body.ok !== false) toast(okMsg, 'success')
        else toast(body.error ?? '操作失败', 'error')
      }
      await onUpdate()
    } finally {
      setBusy(null)
    }
  }

  // 单测：直接用该账号自己的 key 探测上游（绕过 pool 选择器），结果走 toast。
  // 不复用 call()：探测失败时后端返回 error:"" + 非 200 status，要把状态码拼进提示。
  const handleTest = async () => {
    setBusy('test')
    try {
      const r = await apiFetch(`/admin/accounts/${account.id}/test`, { method: 'POST' })
      const b = await r.json().catch(() => ({ ok: false })) as { ok?: boolean; status?: number; error?: string }
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
      const r = await apiFetch(`/admin/accounts/${account.id}`, { method: 'DELETE' })
      toast('已删除账号', r.ok ? 'success' : 'error')
      if (r.ok) await onUpdate()
    } finally {
      setBusy(null)
      setConfirmDelete(false)
    }
  }

  const lastChecked = account.last_checked
    ? new Date(account.last_checked).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
    : '—'

  const disabled = !account.enabled

  // ── Compact row ──────────────────────────────────────────────
  if (compact) return (
    <div className={`flex items-center gap-3 px-4 py-1.5 rounded-lg text-sm ${disabled ? 'bg-ink-800/60 border border-white/[0.06] opacity-75' : 'glass'}`}>
      <StatusDot a={account} />
      <span className={`font-medium w-24 truncate shrink-0 ${disabled ? 'text-gray-500' : ''}`}>{account.alias}</span>
      {account.last_checked
        ? <span className={`font-bold tabular-nums w-16 shrink-0 ${balanceColor(account.balance)}`}>¥{account.balance.toFixed(2)}</span>
        : <span className="font-bold text-gray-400 w-16 shrink-0">—</span>}
      <span className="text-xs text-gray-300 shrink-0">{account.coupon_count}券</span>
      <span className="text-xs text-gray-300 shrink-0">{lastChecked}</span>
      {account.cooldown_secs > 0 && (
        <span className="text-xs text-amber-400 shrink-0 flex items-center gap-1"><Timer className="w-3 h-3" />{account.cooldown_secs}s</span>
      )}
      <div className="flex items-center gap-0.5 ml-auto">
        <button onClick={() => call('refresh', 'refresh', '已刷新余额')} disabled={!!busy} title="查余额" className="p-1.5 rounded-lg text-gray-400 hover:text-white hover:bg-white/10 transition-colors disabled:opacity-40"><RefreshCw className={`w-3.5 h-3.5 ${busy === 'refresh' ? 'animate-spin' : ''}`} /></button>
        <button onClick={handleTest} disabled={!!busy} title="单测" className="p-1.5 rounded-lg text-gray-400 hover:text-white hover:bg-white/10 transition-colors disabled:opacity-40"><FlaskConical className={`w-3.5 h-3.5 ${busy === 'test' ? 'animate-spin' : ''}`} /></button>
        <button onClick={() => call('toggle', 'toggle')} disabled={!!busy} title={account.enabled ? '停用' : '启用'} className={`p-1.5 rounded-lg transition-colors disabled:opacity-40 ${account.enabled ? 'text-rose-300 hover:bg-rose-500/10' : 'text-emerald-300 hover:bg-emerald-500/10'}`}><Power className="w-3.5 h-3.5" /></button>
        {account.cooldown_secs > 0 && (
          <button onClick={() => call('cooldown/clear', 'clear', '已解冻')} disabled={!!busy} title="解冻" className="p-1.5 rounded-lg text-amber-300 hover:bg-amber-500/10 transition-colors disabled:opacity-40"><Timer className="w-3.5 h-3.5" /></button>
        )}
        <button onClick={handleDelete} disabled={!!busy} title={confirmDelete ? '再次点击确认删除' : '删除'} className={`p-1.5 rounded-lg transition-colors disabled:opacity-40 ${confirmDelete ? 'text-rose-300 bg-rose-500/10' : 'text-gray-400 hover:text-white hover:bg-white/10'}`}><Trash2 className="w-3.5 h-3.5" /></button>
      </div>
    </div>
  )

  // ── Card view ─────────────────────────────────────────────────
  return (
    <motion.div
      initial={{ opacity: 0, y: 14 }} animate={{ opacity: 1, y: 0 }}
      transition={{ delay: Math.min(index * 0.04, 0.4) }}
      whileHover={{ y: -4 }}
      className={`rounded-2xl p-5 flex flex-col gap-4 transition-colors ${disabled ? 'bg-ink-800/60 border border-white/[0.06] opacity-75' : 'glass shadow-glass'}`}>
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2 min-w-0">
          <StatusDot a={account} />
          <span className={`font-semibold text-sm truncate ${disabled ? 'text-gray-500' : 'text-gray-100'}`}>{account.alias}</span>
        </div>
        {disabled
          ? <span className="text-[10px] px-2 py-0.5 rounded-full bg-slate-700/40 text-slate-400 border border-white/10 shrink-0">已停用</span>
          : <span className="text-xs text-gray-400 shrink-0">#{account.id}</span>}
      </div>

      <div>
        {account.last_checked ? (
          <AnimatedNumber value={account.balance} decimals={2} prefix="¥"
            className={`block text-3xl font-bold tabular-nums ${balanceColor(account.balance)}`} />
        ) : (
          <span className="text-3xl font-bold text-gray-500">—</span>
        )}
        <div className="text-xs text-gray-400 mt-0.5">
          {account.last_checked ? `${account.coupon_count} 张有效券` : '尚未查询'}
        </div>
      </div>

      <div className="grid grid-cols-2 gap-2">
        <div className="bg-white/[0.04] rounded-lg p-2.5">
          <div className="flex items-center gap-1 text-gray-300 text-xs mb-1"><TrendingUp className="w-3 h-3" />请求数</div>
          <div className="font-mono text-sm font-medium text-gray-100">{account.request_count.toLocaleString()}</div>
        </div>
        <div className="bg-white/[0.04] rounded-lg p-2.5">
          <div className="text-gray-300 text-xs mb-1">更新于</div>
          <div className="text-sm font-medium text-gray-100">{lastChecked}</div>
        </div>
      </div>

      {account.cooldown_secs > 0 && (
        <div className="flex items-center gap-2 bg-amber-500/10 border border-amber-500/20 rounded-lg px-3 py-2 text-xs text-amber-300">
          <CooldownRing secs={account.cooldown_secs} />限流冷却 {account.cooldown_secs}s
        </div>
      )}

      <div className="flex items-center gap-1 pt-1 border-t border-white/10">
        <button onClick={() => call('refresh', 'refresh', '已刷新余额')} disabled={!!busy} title="查余额"
          className="p-2 rounded-lg text-gray-400 hover:text-white hover:bg-white/10 transition-colors disabled:opacity-40">
          <RefreshCw className={`w-4 h-4 ${busy === 'refresh' ? 'animate-spin' : ''}`} />
        </button>
        <button onClick={() => call('toggle', 'toggle', account.enabled ? '已停用' : '已启用')} disabled={!!busy} title={account.enabled ? '停用' : '启用'}
          className={`p-2 rounded-lg transition-colors disabled:opacity-40 ${account.enabled ? 'text-rose-300 hover:bg-rose-500/10' : 'text-emerald-300 hover:bg-emerald-500/10'}`}>
          <Power className="w-4 h-4" />
        </button>
        <button onClick={handleTest} disabled={!!busy} title="单测"
          className="p-2 rounded-lg text-gray-400 hover:text-white hover:bg-white/10 transition-colors disabled:opacity-40">
          <FlaskConical className={`w-4 h-4 ${busy === 'test' ? 'animate-spin' : ''}`} />
        </button>
        {account.cooldown_secs > 0 && (
          <button onClick={() => call('cooldown/clear', 'clear', '已解冻')} disabled={!!busy} title="解冻"
            className="p-2 rounded-lg text-amber-300 hover:bg-amber-500/10 transition-colors disabled:opacity-40">
            <Timer className="w-4 h-4" />
          </button>
        )}
        <button onClick={handleDelete} disabled={!!busy} title={confirmDelete ? '再次点击确认删除' : '删除'}
          className={`p-2 rounded-lg transition-colors disabled:opacity-40 ml-auto ${confirmDelete ? 'bg-rose-600 text-white hover:bg-rose-500' : 'text-rose-300 hover:bg-rose-500/10'}`}>
          <Trash2 className="w-4 h-4" />
        </button>
      </div>
    </motion.div>
  )
})
