export interface AccountStatus {
  index: number
  alias: string
  enabled: boolean
  available: boolean
  balance: number
  coupon_count: number
  last_checked: string
  cooldown_secs: number
  request_count: number
}
