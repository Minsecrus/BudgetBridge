import { useState } from 'react'
import { apiFetch } from '../api'
import { toast } from './Toast'
import { GlassModal } from './ui/GlassModal'

const FIELDS = [
  { key: 'alias',     label: '账号别名（选填，留空显示 sk- 前缀）', placeholder: '留空则用 sk- 前缀', type: 'text',     required: false },
  { key: 'api_key',   label: 'API Key',                        placeholder: 'sk-...', type: 'password', required: true  },
  { key: 'ak_id',     label: 'AccessKey ID',                   placeholder: 'LTAI5...', type: 'text',   required: true  },
  { key: 'ak_secret', label: 'AccessKey Secret',               placeholder: '••••••••', type: 'password', required: true },
  { key: 'ws_domain', label: '业务空间专属域名（选填，留空用全局上游）', placeholder: 'ws-xxxxx.cn-beijing.maas.aliyuncs.com', type: 'text', required: false },
] as const

type FormKey = typeof FIELDS[number]['key']

const inputCls = 'w-full bg-white/5 border border-white/10 focus:border-violet-400 rounded-lg px-3 py-2 text-sm outline-none placeholder-gray-600 transition-colors'

export function AddAccountModal({ onClose, onAdded }: { onClose: () => void; onAdded: () => Promise<void> }) {
  const [form, setForm] = useState<Record<FormKey, string>>({ alias: '', api_key: '', ak_id: '', ak_secret: '', ws_domain: '' })
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  // Smart paste: drop a whole credential block anywhere and it auto-fills by
  // prefix — lines starting with "sk" → API Key, "LTAI" → AccessKey ID,
  // "ws-" (strict lowercase, with hyphen), "http(s)://", or "llm-" → 业务空间专属域名, the remaining non-empty
  // line → AccessKey Secret.
  const handlePaste = (e: React.ClipboardEvent) => {
    const text = e.clipboardData.getData('text')
    if (!text) return
    const lines = text.split(/\r?\n/).map(l => l.trim()).filter(Boolean)
    if (!lines.some(l => {
      const low = l.toLowerCase()
      return low.startsWith('sk') || l.startsWith('LTAI') || l.startsWith('ws-') || low.startsWith('http') || low.startsWith('llm-')
    })) return
    e.preventDefault()
    let apiKey = '', akId = '', akSecret = '', wsDomain = ''
    for (const line of lines) {
      const low = line.toLowerCase()
      if (!apiKey && low.startsWith('sk')) apiKey = line
      else if (!akId && line.startsWith('LTAI')) akId = line
      else if (!wsDomain && (line.startsWith('ws-') || low.startsWith('http') || low.startsWith('llm-'))) wsDomain = line
      else if (!akSecret) akSecret = line
    }
    setForm(f => ({
      ...f,
      ...(apiKey ? { api_key: apiKey } : null),
      ...(akId ? { ak_id: akId } : null),
      ...(wsDomain ? { ws_domain: wsDomain } : null),
      ...(akSecret ? { ak_secret: akSecret } : null),
    }))
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setLoading(true)
    setError('')
    try {
      const res = await apiFetch('/admin/accounts', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(form),
      })
      if (!res.ok) {
        const msg = ((await res.json()) as { error: string }).error ?? '添加失败'
        setError(msg)
        toast(msg, 'error')
        return
      }
      toast('已添加账号')
      await onAdded()
      onClose()
    } catch {
      setError('网络错误')
      toast('网络错误', 'error')
    } finally {
      setLoading(false)
    }
  }

  return (
    <GlassModal onClose={onClose} title="添加账号" backdropClose={false}>
      <form onSubmit={submit} onPaste={handlePaste} className="flex flex-col gap-3">
        {FIELDS.map(({ key, label, placeholder, type, required }) => (
          <div key={key}>
            <label className="block text-xs text-gray-300 mb-1">{label}</label>
            <input
              type={type}
              required={required}
              value={form[key]}
              onChange={e => setForm(f => ({ ...f, [key]: e.target.value }))}
              placeholder={placeholder}
              className={inputCls}
            />
          </div>
        ))}
        {error && <p className="text-rose-300 text-xs">{error}</p>}
        <button type="submit" disabled={loading}
          className="mt-1 bg-violet-600 hover:bg-violet-500 disabled:opacity-50 disabled:cursor-not-allowed rounded-lg py-2 text-sm font-medium transition-colors">
          {loading ? '添加中…' : '添加账号'}
        </button>
      </form>
    </GlassModal>
  )
}
