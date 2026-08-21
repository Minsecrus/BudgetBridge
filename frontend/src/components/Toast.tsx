import { useEffect, useState } from 'react'
import { CheckCircle2, XCircle } from 'lucide-react'

type ToastItem = { id: number; msg: string; type: 'success' | 'error' }

// toast dispatches a custom event; ToastContainer (mounted once in App) shows it.
export function toast(msg: string, type: 'success' | 'error' = 'success') {
  window.dispatchEvent(new CustomEvent('bb:toast', { detail: { msg, type } }))
}

let nextID = 1

export function ToastContainer() {
  const [items, setItems] = useState<ToastItem[]>([])

  useEffect(() => {
    const handler = (e: Event) => {
      const detail = (e as CustomEvent).detail as { msg: string; type: 'success' | 'error' }
      const id = nextID++
      setItems(prev => [...prev, { id, msg: detail.msg, type: detail.type }])
      setTimeout(() => setItems(prev => prev.filter(t => t.id !== id)), 2500)
    }
    window.addEventListener('bb:toast', handler)
    return () => window.removeEventListener('bb:toast', handler)
  }, [])

  return (
    <div className="fixed top-4 right-4 z-[100] flex flex-col gap-2 pointer-events-none">
      {items.map(t => (
        <div key={t.id} className={`flex items-center gap-2 rounded-lg px-4 py-2.5 text-sm shadow-lg border backdrop-blur ${
          t.type === 'success'
            ? 'bg-green-500/15 border-green-500/30 text-green-300'
            : 'bg-red-500/15 border-red-500/30 text-red-300'
        }`}>
          {t.type === 'success'
            ? <CheckCircle2 className="w-4 h-4 shrink-0" />
            : <XCircle className="w-4 h-4 shrink-0" />}
          <span className="whitespace-nowrap">{t.msg}</span>
        </div>
      ))}
    </div>
  )
}
