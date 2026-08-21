import { Copy, Check, Zap, Plus, FlaskConical, Trash2, LayoutGrid, List, LogOut, RefreshCw, KeyRound } from 'lucide-react'
import { useState, useEffect } from 'react'
import { motion, AnimatePresence } from 'framer-motion'
import type { AccountStatus } from '../types'
import { AddAccountModal } from './AddAccountModal'
import { TestAllModal } from './TestAllModal'
import { apiFetch, clearToken } from '../api'
import { toast } from './Toast'

export function TopBar({ accounts, onUpdate, compact, onToggleCompact }: {
  accounts: AccountStatus[]
  onUpdate: () => Promise<void>
  compact: boolean
  onToggleCompact: () => void
}) {
  const [copied, setCopied] = useState(false)
  const [copiedKey, setCopiedKey] = useState(false)
  const [showAdd, setShowAdd] = useState(false)
  const [showTest, setShowTest] = useState(false)
  const [clearConfirm, setClearConfirm] = useState(false)
  const [rotateConfirm, setRotateConfirm] = useState(false)
  const [apiFormat, setApiFormat] = useState<'openai' | 'anthropic'>('openai')
  const [baseUrl, setBaseUrl] = useState('')
  const [apiKey, setApiKey] = useState('')

  useEffect(() => {
    apiFetch('/admin/config')
      .then(r => r.ok ? r.json() : null)
      .then(data => setBaseUrl(data?.public_url || window.location.origin))
      .catch(() => setBaseUrl(window.location.origin))
    apiFetch('/admin/api-key')
      .then(r => r.ok ? r.json() : null)
      .then(data => setApiKey(data?.api_key ?? ''))
      .catch(() => setApiKey(''))
  }, [])

  const endpoint = apiFormat === 'openai' ? `${baseUrl}/v1` : baseUrl
  const apiLabel = apiFormat === 'openai' ? 'OpenAI' : 'Anthropic'
  const active = accounts.filter(a => a.available).length

  const copy = async () => {
    await navigator.clipboard.writeText(endpoint)
    setCopied(true); toast('已复制端点地址'); setTimeout(() => setCopied(false), 1500)
  }
  const copyKey = async () => {
    if (!apiKey) return
    await navigator.clipboard.writeText(apiKey)
    setCopiedKey(true); toast('已复制 API Key'); setTimeout(() => setCopiedKey(false), 1500)
  }
  const rotateKey = async () => {
    if (!rotateConfirm) { setRotateConfirm(true); setTimeout(() => setRotateConfirm(false), 3000); return }
    try {
      const r = await apiFetch('/admin/api-key/rotate', { method: 'POST' })
      const data = await r.json()
      setApiKey(data.api_key ?? '')
      toast('API Key 已轮换')
    } catch { /* ignore */ }
    setRotateConfirm(false)
  }
  const toggleApiFormat = () => setApiFormat(prev => prev === 'openai' ? 'anthropic' : 'openai')
  const clearAll = async () => {
    if (!clearConfirm) { setClearConfirm(true); setTimeout(() => setClearConfirm(false), 3000); return }
    const r = await apiFetch('/admin/accounts', { method: 'DELETE' })
    setClearConfirm(false)
    toast('已清空', r.ok ? 'success' : 'error')
    if (r.ok) await onUpdate()
  }
  const logout = () => { clearToken(); window.dispatchEvent(new Event('bb:unauthorized')) }

  return (
    <>
      <header className="sticky top-0 z-20 glass border-b border-white/10">
        <div className="max-w-7xl mx-auto px-6 py-3 flex flex-wrap items-center justify-between gap-4">
          <div className="flex items-center gap-3 shrink-0">
            <motion.div whileHover={{ rotate: 15 }}>
              <Zap className="w-5 h-5 text-amber-300 drop-shadow-[0_0_8px_rgba(251,191,36,.6)]" />
            </motion.div>
            <span className="font-bold text-lg tracking-tight text-gradient">BudgetBridge</span>
            <span className="text-xs bg-white/10 text-gray-200 px-2 py-0.5 rounded-full">{active}/{accounts.length} 可用</span>
          </div>

          <div className="flex items-center gap-2 flex-1 basis-[320px] min-w-[280px]">
            <button onClick={toggleApiFormat} title={`切换为 ${apiFormat === 'openai' ? 'Anthropic' : 'OpenAI'} 格式`}
              className="shrink-0 px-2 py-2 rounded-lg bg-white/5 hover:bg-white/10 text-xs font-medium text-gray-200 transition-colors">{apiLabel}</button>
            <button onClick={copy} title="复制端点地址"
              className="flex items-center gap-2 bg-white/5 hover:bg-white/10 rounded-lg px-3 py-2 transition-colors group flex-1 min-w-0">
              <span className="text-xs font-mono text-gray-300 truncate">{endpoint}</span>
              {copied ? <Check className="w-3.5 h-3.5 text-emerald-400 shrink-0" /> : <Copy className="w-3.5 h-3.5 text-gray-400 group-hover:text-gray-200 shrink-0 transition-colors" />}
            </button>
            <button onClick={copyKey} title="复制 API Key"
              className="flex items-center gap-2 bg-white/5 hover:bg-white/10 rounded-lg px-3 py-2 transition-colors group flex-1 min-w-0">
              <KeyRound className="w-3.5 h-3.5 text-gray-400 shrink-0" />
              <span className="text-xs font-mono text-gray-300 truncate">{apiKey || '未配置'}</span>
              {copiedKey ? <Check className="w-3.5 h-3.5 text-emerald-400 shrink-0" /> : <Copy className="w-3.5 h-3.5 text-gray-400 group-hover:text-gray-200 shrink-0 transition-colors" />}
            </button>
            <button onClick={rotateKey} title={rotateConfirm ? '再次点击确认轮换（旧 Key 立即失效）' : '轮换 API Key'}
              className={`shrink-0 flex items-center justify-center w-9 h-9 rounded-lg transition-colors ${rotateConfirm ? 'bg-amber-600 hover:bg-amber-500 text-white' : 'bg-white/5 hover:bg-white/10 text-gray-300'}`}>
              <RefreshCw className="w-4 h-4" />
            </button>
          </div>

          <div className="flex items-center gap-2 shrink-0">
            <button onClick={onToggleCompact} title={compact ? '卡片视图' : '紧凑视图'}
              className="flex items-center justify-center w-9 h-9 bg-white/5 hover:bg-white/10 rounded-lg text-gray-300 transition-colors">
              {compact ? <LayoutGrid className="w-4 h-4" /> : <List className="w-4 h-4" />}
            </button>
            <button onClick={() => setShowTest(true)}
              className="flex items-center gap-1.5 bg-white/5 hover:bg-white/10 rounded-lg px-3 py-2 text-sm font-medium text-gray-300 transition-colors">
              <FlaskConical className="w-4 h-4" />普测
            </button>
            <button onClick={clearAll}
              className={`flex items-center gap-1.5 rounded-lg px-3 py-2 text-sm font-medium transition-colors ${clearConfirm ? 'bg-rose-600 hover:bg-rose-500 text-white' : 'bg-white/5 hover:bg-white/10 text-gray-300'}`}>
              <Trash2 className="w-4 h-4" />{clearConfirm ? '确认清空？' : '清空'}
            </button>
            <button onClick={() => setShowAdd(true)}
              className="flex items-center gap-1.5 bg-violet-600 hover:bg-violet-500 rounded-lg px-3 py-2 text-sm font-medium transition-colors shadow-glow-violet">
              <Plus className="w-4 h-4" />添加账号
            </button>
            <button onClick={logout} title="退出登录"
              className="flex items-center justify-center w-9 h-9 bg-white/5 hover:bg-white/10 rounded-lg text-gray-400 hover:text-white transition-colors">
              <LogOut className="w-4 h-4" />
            </button>
          </div>
        </div>
      </header>

      <AnimatePresence>
        {showAdd && <AddAccountModal key="add" onClose={() => setShowAdd(false)} onAdded={onUpdate} />}
        {showTest && <TestAllModal key="test" onClose={() => setShowTest(false)} onUpdate={onUpdate} />}
      </AnimatePresence>
    </>
  )
}
