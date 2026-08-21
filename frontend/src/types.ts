export interface AccountStatus {
  id: number
  alias: string
  enabled: boolean
  available: boolean
  balance: number
  coupon_count: number
  last_checked: string
  cooldown_secs: number
  request_count: number
}

// A fallback channel: a non-Alibaba OpenAI-compatible endpoint used only when
// the account pool is exhausted. No balance, no cooldown, no availability —
// just a model whitelist gating when it may back up the pool. api_key is never
// shipped back by the list endpoint (one-way, like AccountStatus).
export interface FallbackChannel {
  id: number
  name: string
  base_url: string
  models: string[]
  enabled: boolean
  disabled_by_err?: boolean
}

export interface TimelinePoint { ts: number; req: number; ok: number; err: number; r429: number; net_retry: number }

export interface GlobalStats {
  total_balance: number
  available: number
  total: number
  requests_total: number
  requests_window: number
  success_rate: number
  errors_window: number
  throttled_429_window: number
  network_retries_window: number
}

export interface BalancePoint { ts: number; balance: number; coupons: number }

export interface AccountStat {
  alias: string
  requests_window: number
  success_rate: number
  throttled_window: number
  balance: number
  balance_history: BalancePoint[]
  enabled: boolean
  available: boolean
  cooldown_secs: number
}

export interface StatsResponse {
  window: string
  global: GlobalStats
  timeline: TimelinePoint[]
  per_account: AccountStat[]
}

export type Window = '1h' | '24h' | '7d'

export interface RequestAttempt {
  account_id: number
  alias: string
  status: number
  outcome: string
  dur_ms: number
  err?: string
}

export interface RequestLog {
  id: string
  ts: number
  proto: 'openai' | 'anthropic'
  model: string
  stream: boolean
  bytes: number
  key_hash: string
  warm: boolean
  cold: boolean
  attempts: RequestAttempt[]
  retries: number
  final_account_id: number
  final_alias: string
  final_status: number
  outcome: string
  dur_ms: number
  ttft_ms?: number
  prompt_tokens?: number
  completion_tokens?: number
  total_tokens?: number
  cached_tokens?: number
}
