import { X, Send, Loader2, CheckCircle2, AlertCircle } from 'lucide-react'
import { useState } from 'react'

type ReqState = 'idle' | 'loading' | 'ok' | 'error'

export function TestModal({ onClose }: { onClose: () => void }) {
  const [model, setModel] = useState('qwen-turbo')
  const [prompt, setPrompt] = useState('你好，请用一句话介绍自己。')
  const [response, setResponse] = useState('')
  const [reqState, setReqState] = useState<ReqState>('idle')

  const run = async () => {
    setReqState('loading')
    setResponse('')
    try {
      const res = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer test' },
        body: JSON.stringify({
          model,
          messages: [{ role: 'user', content: prompt }],
          stream: false,
        }),
      })
      const data = await res.json()
      if (!res.ok) {
        setReqState('error')
        setResponse(data?.error?.message ?? JSON.stringify(data, null, 2))
      } else {
        setReqState('ok')
        setResponse(data?.choices?.[0]?.message?.content ?? JSON.stringify(data, null, 2))
      }
    } catch (e) {
      setReqState('error')
      setResponse(String(e))
    }
  }

  const isOk = reqState === 'ok'

  return (
    <div
      className="fixed inset-0 bg-black/60 backdrop-blur-sm flex items-center justify-center z-50 p-4"
      onClick={e => e.target === e.currentTarget && onClose()}
    >
      <div className="bg-gray-900 border border-gray-700 rounded-xl w-full max-w-lg p-6 flex flex-col gap-4">
        <div className="flex items-center justify-between">
          <h2 className="font-semibold text-base">端到端测试</h2>
          <button onClick={onClose} className="text-gray-500 hover:text-white transition-colors">
            <X className="w-4 h-4" />
          </button>
        </div>

        <div>
          <label className="block text-xs text-gray-400 mb-1">模型</label>
          <input
            value={model}
            onChange={e => setModel(e.target.value)}
            className="w-full bg-gray-800 border border-gray-700 focus:border-gray-500 rounded-lg px-3 py-2 text-sm outline-none transition-colors"
          />
        </div>

        <div>
          <label className="block text-xs text-gray-400 mb-1">提示词</label>
          <textarea
            rows={3}
            value={prompt}
            onChange={e => setPrompt(e.target.value)}
            className="w-full bg-gray-800 border border-gray-700 focus:border-gray-500 rounded-lg px-3 py-2 text-sm outline-none resize-none transition-colors"
          />
        </div>

        <button
          onClick={run}
          disabled={reqState === 'loading'}
          className="flex items-center justify-center gap-2 bg-indigo-600 hover:bg-indigo-500 disabled:opacity-50 disabled:cursor-not-allowed rounded-lg py-2 text-sm font-medium transition-colors"
        >
          {reqState === 'loading'
            ? <><Loader2 className="w-4 h-4 animate-spin" />请求中…</>
            : <><Send className="w-4 h-4" />发送测试请求</>}
        </button>

        {response && (
          <div className={`rounded-lg p-3 text-sm border ${isOk ? 'bg-green-500/10 border-green-500/20' : 'bg-red-500/10 border-red-500/20'}`}>
            <div className={`flex items-center gap-1.5 mb-2 text-xs font-medium ${isOk ? 'text-green-400' : 'text-red-400'}`}>
              {isOk ? <CheckCircle2 className="w-3.5 h-3.5" /> : <AlertCircle className="w-3.5 h-3.5" />}
              {isOk ? '请求成功' : '请求失败'}
            </div>
            <p className="text-gray-300 whitespace-pre-wrap leading-relaxed text-xs">{response}</p>
          </div>
        )}
      </div>
    </div>
  )
}
