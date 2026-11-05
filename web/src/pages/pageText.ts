// Small pure helpers for the account, admin and recordings pages (ux1101 B4):
// what a host form lacks, how an audit entry and a time read. No React here,
// so they can be tested on their own.
import { getLang, hasKey, relTime, t, type I18nKey } from '../i18n'

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
// An entry's details as "key: value" pairs, one a line, in words where the
// key is known (pages-1105 3.2): "时长：26 秒", a session's long id cut to
// its first eight (the whole stays in the entry's own text); long values cut.
const LONG_ID = /^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/i
export function auditDetail(detail: Record<string, unknown> | null | undefined): string[] {
  if (!detail) return []
  return Object.entries(detail).map(([k, v]) => {
    const key = 'auditKey.' + k
    let s: string
    if (k === 'seconds' && typeof v === 'number') s = durationText(v)
    else if (k === 'bytes' && typeof v === 'number') s = sizeText(v)
    else if (typeof v === 'boolean') s = t(v ? 'auditDetail.yes' : 'auditDetail.no')
    else if (typeof v === 'string' && LONG_ID.test(v)) s = v.slice(0, 8)
    else s = typeof v === 'string' ? v : JSON.stringify(v)
    if (s.length > 80) s = s.slice(0, 79) + '…'
    return (hasKey(key) ? t(key as I18nKey) : k) + t('auditDetail.sep') + s
  })
}
// (A size in words, as the files panel says it.)
function sizeText(n: number): string {
  const units = ['B', 'KB', 'MB', 'GB']
  let i = 0
  let x = n
  while (x >= 1024 && i < units.length - 1) {
    x /= 1024
    i++
  }
  return (i === 0 ? String(x) : x.toFixed(x < 10 ? 1 : 0)) + ' ' + units[i]
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

// A day's heading in a list grouped by day: "今天", "昨天", else the date
// ("11月3日", the year too when it is not this one) — by this device's days.
export function dayHeading(unix: number, now = new Date()): string {
  const d = new Date(unix * 1000)
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime()
  const diff = Math.round((day(now) - day(d)) / 86400000)
  if (diff === 0) return t('day.today')
  if (diff === 1) return t('day.yesterday')
  return new Intl.DateTimeFormat(getLang(), d.getFullYear() === now.getFullYear() ? { month: 'short', day: 'numeric' } : { year: 'numeric', month: 'short', day: 'numeric' }).format(d)
}

// Items in order, grouped by the day (this device's) each is of; each day once.
export function byDay<T>(items: T[], at: (x: T) => number): { key: string; heading: string; items: T[] }[] {
  const out: { key: string; heading: string; items: T[] }[] = []
  for (const x of items) {
    const d = new Date(at(x) * 1000)
    const key = `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`
    const last = out[out.length - 1]
    if (last && last.key === key) last.items.push(x)
    else out.push({ key, heading: dayHeading(at(x)), items: [x] })
  }
  return out
}

// The start of a span of days back from today, by this device's days (unix
// seconds): "today" its midnight, "7d" the midnight six days before…; 0: all.
export type DaySpan = 'all' | 'today' | '7d' | '30d'
export function spanFrom(span: DaySpan, now = new Date()): number {
  if (span === 'all') return 0
  const back = span === 'today' ? 0 : span === '7d' ? 6 : 29
  return Math.floor(new Date(now.getFullYear(), now.getMonth(), now.getDate() - back).getTime() / 1000)
}

// A time of day, for a row under its day's heading: "14:05".
export function clockText(unix: number): string {
  const d = new Date(unix * 1000)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}
