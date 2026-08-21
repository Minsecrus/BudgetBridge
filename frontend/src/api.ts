import type { FallbackChannel, RequestLog, StatsResponse, Window } from './types'

export const getToken = () => localStorage.getItem('admin_token') ?? ''
export const setToken = (t: string) => localStorage.setItem('admin_token', t)
export const clearToken = () => localStorage.removeItem('admin_token')

export async function apiFetch(url: string, options?: RequestInit): Promise<Response> {
  const res = await fetch(url, {
    ...options,
    headers: { ...options?.headers, Authorization: `Bearer ${getToken()}` },
  })
  if (res.status === 401) {
    clearToken()
    window.dispatchEvent(new Event('bb:unauthorized'))
  }
  return res
}

export async function fetchStats(window: Window): Promise<StatsResponse | null> {
  const r = await apiFetch(`/admin/stats?window=${window}`)
  return r.ok ? r.json() : null
}

export interface StreamState {
  connected: boolean
}

// streamEvents subscribes to the live request-log SSE feed. We can't use
// EventSource because it won't send the Bearer auth header, so we fetch the
// stream and parse SSE frames (data: …\n\n) off the ReadableStream manually.
// On disconnect it reconnects with exponential backoff (capped 30s); on 401 it
// surfaces the unauthorized event like apiFetch. Returns a stop() function.
export function streamEvents(
  onLog: (log: RequestLog) => void,
  onState: (s: StreamState) => void,
  replayLimit?: number,
): () => void {
  let stopped = false
  let attempt = 0
  let controller: AbortController | null = null

  const connect = async () => {
    if (stopped) return
    controller = new AbortController()
    try {
      const url = replayLimit ? `/admin/events?limit=${replayLimit}` : '/admin/events'
      const res = await fetch(url, {
        headers: { Authorization: `Bearer ${getToken()}` },
        signal: controller.signal,
      })
      if (res.status === 401) {
        clearToken()
        window.dispatchEvent(new Event('bb:unauthorized'))
        return
      }
      if (!res.ok || !res.body) {
        onState({ connected: false })
        reconnect()
        return
      }
      onState({ connected: true })
      attempt = 0
      const reader = res.body.getReader()
      const dec = new TextDecoder()
      let buf = ''
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buf += dec.decode(value, { stream: true })
        let idx: number
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const block = buf.slice(0, idx)
          buf = buf.slice(idx + 2)
          const data = block
            .split('\n')
            .filter((l) => l.startsWith('data:'))
            .map((l) => l.slice(5).trimStart())
            .join('\n')
          if (!data) continue // heartbeat comment
          try {
            onLog(JSON.parse(data) as RequestLog)
          } catch {
            /* skip malformed frame */
          }
        }
      }
    } catch {
      /* aborted or network error */
    }
    if (!stopped) {
      onState({ connected: false })
      reconnect()
    }
  }

  const reconnect = () => {
    if (stopped) return
    attempt++
    const delay = Math.min(30_000, 500 * 2 ** Math.min(attempt, 6))
    setTimeout(connect, delay)
  }

  connect()
  return () => {
    stopped = true
    controller?.abort()
  }
}

export async function fetchLogs(params: {
  before?: number
  limit?: number
  account?: number
  outcome?: string
  q?: string
}): Promise<RequestLog[]> {
  const qs = new URLSearchParams()
  if (params.before) qs.set('before', String(params.before))
  if (params.limit) qs.set('limit', String(params.limit))
  if (params.account) qs.set('account', String(params.account))
  if (params.outcome) qs.set('outcome', params.outcome)
  if (params.q) qs.set('q', params.q)
  const r = await apiFetch(`/admin/logs?${qs.toString()}`)
  if (!r.ok) return []
  const j = await r.json()
  return (j.logs ?? []) as RequestLog[]
}

// ── Fallback channels (pool-exhausted backup) ───────────────────────────────
// Named functions rather than inline apiFetch in the component: the account
// vertical scattered its calls across AccountCard/AddAccountModal/TopBar; a
// second entity of the same shape gets a small API layer up front to keep the
// components thin. 401 is handled centrally by apiFetch (bb:unauthorized).

export async function listFallbacks(): Promise<FallbackChannel[] | null> {
  const r = await apiFetch('/admin/fallbacks')
  return r.ok ? r.json() : null
}

export async function addFallback(cfg: {
  name: string
  base_url: string
  api_key: string
  models: string[]
}): Promise<{ ok: boolean; error?: string }> {
  const r = await apiFetch('/admin/fallbacks', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(cfg),
  })
  if (!r.ok) {
    const msg = ((await r.json().catch(() => ({}))) as { error?: string }).error ?? '添加失败'
    return { ok: false, error: msg }
  }
  return { ok: true }
}

export async function deleteFallback(id: number): Promise<boolean> {
  const r = await apiFetch(`/admin/fallbacks/${id}`, { method: 'DELETE' })
  return r.ok
}

// Update a channel's editable fields. api_key "" means "keep the existing key"
// (the edit form never receives the secret back, so a blank field = no rotation).
export async function updateFallback(id: number, cfg: {
  name: string
  base_url: string
  api_key: string
  models: string[]
}): Promise<{ ok: boolean; error?: string }> {
  const r = await apiFetch(`/admin/fallbacks/${id}/update`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(cfg),
  })
  if (!r.ok) {
    const msg = ((await r.json().catch(() => ({}))) as { error?: string }).error ?? '保存失败'
    return { ok: false, error: msg }
  }
  return { ok: true }
}

export async function toggleFallback(id: number): Promise<boolean> {
  const r = await apiFetch(`/admin/fallbacks/${id}/toggle`, { method: 'POST' })
  return r.ok
}

export async function testFallback(id: number): Promise<{ ok: boolean; status?: number; error?: string }> {
  const r = await apiFetch(`/admin/fallbacks/${id}/test`, { method: 'POST' })
  return r.json().catch(() => ({ ok: false }))
}
