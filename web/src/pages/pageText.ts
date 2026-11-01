// Small pure helpers for the account, admin and recordings pages (ux1101 B4):
// what a host form lacks, how an audit entry and a time read. No React here,
// so they can be tested on their own.
import { hasKey, relTime, t, type I18nKey } from '../i18n'

// The host form, as the server checks it (internal/web/api_hosts.go
// validateHost), in the form's order: every field at fault at once, each
// with its message's key. The server's own answer stays the last word (a
// key that is no longer there, a jump host that cannot be one).
export type HostFormValues = { name: string; address: string; port: string; username: string; auth: string; credentialId: number | null; password?: string; hadPassword?: boolean }
const HOSTNAME = /^[A-Za-z0-9_]([A-Za-z0-9._-]{0,251}[A-Za-z0-9_])?$/
export function hostFormErrors(v: HostFormValues): [field: string, key: I18nKey][] {
  const out: [string, I18nKey][] = []
  const name = v.name.trim()
  if (!name || [...name].length > 64) out.push(['name', 'hostField.name'])
  const addr = v.address.trim().replace(/^\[|\]$/g, '')
  if (!HOSTNAME.test(addr) && !addr.includes(':')) out.push(['address', 'hostField.address'])
  const port = v.port === '' ? 22 : Number(v.port)
  if (!(Number.isInteger(port) && port >= 1 && port <= 65535)) out.push(['port', 'host.portInvalid'])
  // (As the server's: Go's \s is ASCII white space only; 255 characters, not UTF-16 units.)
  const user = v.username.trim()
  if (!user || [...user].length > 255 || /[ \t\n\v\f\r\x00-\x1f\x7f]/.test(user)) out.push(['username', 'hostField.username'])
  if (v.auth === 'key' && v.credentialId === null) out.push(['credential_id', 'hostField.credential_id'])
  // (A password to save, none kept before: one must be given — review.)
  if (v.auth === 'password' && v.password !== undefined && !v.password && !v.hadPassword) out.push(['password', 'hostField.password'])
  return out
}

// An audit entry's action in words ("convo.screen" → "读取智能体屏幕"); one
// without a name of its own is shown as it is.
export function auditAction(action: string): string {
  const k = 'auditAction.' + action
  return hasKey(k) ? t(k) : action
}

const HEX_ID = /^[0-9a-f]{32}$/
// Session ids in the entries shown, to the host each was opened on (the
// target of an entry that names the host, and the session in its details).
export function sessionHosts(entries: { target: string; detail?: Record<string, unknown> | null }[]): Map<string, string> {
  const m = new Map<string, string>()
  for (const e of entries) {
    const s = e.detail?.session
    if (typeof s === 'string' && e.target && !HEX_ID.test(e.target)) m.set(s, e.target.replace(/ \(.*\)$/, ''))
  }
  return m
}
// An entry's target as read: a session id becomes its host where the page
// knows it, else its first eight characters (the whole in a title).
export function auditTarget(target: string, hosts: Map<string, string>): string {
  if (!HEX_ID.test(target)) return target
  return hosts.get(target) ?? target.slice(0, 8) + '…'
}
// An entry's details as "key: value" pairs, one a line; long values cut.
export function auditDetail(detail: Record<string, unknown> | null | undefined): string[] {
  if (!detail) return []
  return Object.entries(detail).map(([k, v]) => {
    let s = typeof v === 'string' ? v : JSON.stringify(v)
    if (s.length > 80) s = s.slice(0, 79) + '…'
    return k + t('auditDetail.sep') + s
  })
}

// A time as a list shows it: "3 分钟前" within a day, then the date and the
// time of day ("10-28 14:05", the year too when it is not this one).
export function whenText(unix: number, now = Date.now()): string {
  if (!unix || now / 1000 - unix < 86400) return relTime(unix)
  const d = new Date(unix * 1000)
  const pad = (n: number) => String(n).padStart(2, '0')
  const day = `${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
  const year = d.getFullYear() !== new Date(now).getFullYear() ? d.getFullYear() + '-' : ''
  return `${year}${day} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

// A length of time in words: "13 小时 10 分", "4 分 12 秒", "12 秒".
export function durationText(secs: number): string {
  const s = Math.max(0, Math.floor(secs))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (h > 0) return t('dur.hm', { h, m })
  if (m > 0) return t('dur.ms', { m, s: s % 60 })
  return t('dur.s', { s })
}
