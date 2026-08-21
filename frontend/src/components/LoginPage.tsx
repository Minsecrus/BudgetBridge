import { useState } from 'react'
import { Zap, Loader2 } from 'lucide-react'
import { motion } from 'framer-motion'
import { setToken } from '../api'

export function LoginPage({ onLogin }: { onLogin: () => void }) {
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setLoading(true)
    setError('')
    try {
      const res = await fetch('/admin/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password }),
      })
      const data = await res.json()
      if (!res.ok) { setError(data.error ?? '登录失败'); return }
      setToken(data.token)
      onLogin()
    } catch {
      setError('网络错误')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen bg-ink-950 flex items-center justify-center p-4 relative overflow-hidden">
      <div className="pointer-events-none fixed inset-0 bg-aurora animate-aurora-float opacity-70" />
      <motion.div
        initial={{ opacity: 0, y: 20, scale: 0.96 }} animate={{ opacity: 1, y: 0, scale: 1 }}
        transition={{ type: 'spring', stiffness: 280, damping: 26 }}
        className="relative w-full max-w-sm glass rounded-2xl p-8 flex flex-col gap-6 shadow-glass">
        <div className="flex items-center gap-2.5">
          <Zap className="w-5 h-5 text-amber-300 drop-shadow-[0_0_8px_rgba(251,191,36,.6)]" />
          <span className="font-bold text-lg tracking-tight text-gradient">BudgetBridge</span>
        </div>
        <form onSubmit={submit} className="flex flex-col gap-4">
          <div>
            <label className="block text-xs text-gray-300 mb-1">管理员密码</label>
            <input
              type="password"
              autoFocus
              value={password}
              onChange={e => setPassword(e.target.value)}
              placeholder="输入密码"
              className="w-full bg-white/5 border border-white/10 focus:border-violet-400 rounded-lg px-3 py-2 text-sm outline-none transition-colors"
            />
          </div>
          {error && <p className="text-rose-300 text-xs">{error}</p>}
          <button type="submit" disabled={loading || !password}
            className="flex items-center justify-center gap-2 bg-violet-600 hover:bg-violet-500 disabled:opacity-50 disabled:cursor-not-allowed rounded-lg py-2 text-sm font-medium transition-colors shadow-glow-violet">
            {loading ? <><Loader2 className="w-4 h-4 animate-spin" />登录中…</> : '登录'}
          </button>
        </form>
      </motion.div>
    </div>
  )
}
