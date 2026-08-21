import { useState } from 'react'
import { addFallback, updateFallback } from '../api'
import { toast } from './Toast'
import { GlassModal } from './ui/GlassModal'
import type { FallbackChannel } from '../types'

const inputCls = 'w-full bg-white/5 border border-white/10 focus:border-violet-400 rounded-lg px-3 py-2 text-sm outline-none placeholder-gray-600 transition-colors'

// FallbackModal handles both adding and editing a fallback channel. Edit mode
// prefills from `channel` and makes api_key optional (blank = keep the existing
// secret, which the list endpoint never returns); add mode requires it. One
// component, two modes — avoids a near-duplicate second modal.
export function FallbackModal({ mode, channel, onClose, onSaved }: {
  mode: 'add' | 'edit'
  channel?: FallbackChannel
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const editing = mode === 'edit'
  const [name, setName] = useState(channel?.name ?? '')
  const [baseURL, setBaseURL] = useState(channel?.base_url ?? '')
  const [apiKey, setApiKey] = useState('')
  const [models, setModels] = useState(channel?.models.join(', ') ?? '')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setLoading(true)
    setError('')
    try {
      const mods = models.split(',').map(s => s.trim()).filter(Boolean)
      const payload = { name: name.trim(), base_url: baseURL.trim(), api_key: apiKey.trim(), models: mods }
      const res = editing && channel
        ? await updateFallback(channel.id, payload)
        : await addFallback(payload)
      if (!res.ok) {
        setError(res.error ?? (editing ? '保存失败' : '添加失败'))
        toast(res.error ?? (editing ? '保存失败' : '添加失败'), 'error')
        return
      }
      toast(editing ? '已保存' : '已添加通道')
      await onSaved()
      onClose()
    } catch {
      setError('网络错误')
      toast('网络错误', 'error')
    } finally {
      setLoading(false)
    }
  }

  return (
    <GlassModal onClose={onClose} title={editing ? '编辑兜底通道' : '添加兜底通道'} backdropClose={false}>
      <form onSubmit={submit} className="flex flex-col gap-3">
        <div>
          <label className="block text-xs text-gray-300 mb-1">通道名称</label>
          <input type="text" value={name} onChange={e => setName(e.target.value)}
            placeholder="OpenRouter" className={inputCls} />
        </div>
        <div>
          <label className="block text-xs text-gray-300 mb-1">Base URL（含 /v1）</label>
          <input type="text" required value={baseURL} onChange={e => setBaseURL(e.target.value)}
            placeholder="https://openrouter.ai/api/v1" className={inputCls} />
        </div>
        <div>
          <label className="block text-xs text-gray-300 mb-1">
            API Key{editing ? '（留空则不修改）' : ''}
          </label>
          <input type="password" required={!editing} value={apiKey} onChange={e => setApiKey(e.target.value)}
            placeholder={editing ? '••••••（留空保持不变）' : 'sk-...'} className={inputCls} />
        </div>
        <div>
          <label className="block text-xs text-gray-300 mb-1">可用模型（逗号分隔；* = 通配；留空 = 不匹配任何请求）</label>
          <input type="text" value={models} onChange={e => setModels(e.target.value)}
            placeholder="qwen-plus, gpt-4o" className={inputCls} />
        </div>
        {error && <p className="text-rose-300 text-xs">{error}</p>}
        <button type="submit" disabled={loading}
          className="mt-1 bg-violet-600 hover:bg-violet-500 disabled:opacity-50 disabled:cursor-not-allowed rounded-lg py-2 text-sm font-medium transition-colors">
          {loading ? (editing ? '保存中…' : '添加中…') : (editing ? '保存' : '添加通道')}
        </button>
      </form>
    </GlassModal>
  )
}
