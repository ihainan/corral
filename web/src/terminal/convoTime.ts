import { t } from '../i18n'

// How the conversation and the lists say time: one place, so that a row,
// the bar and a turn's line say the same thing the same way.

// A duration as the view says it: 45 s, 2 min 14 s, 1 h 5 min; past two
// days, in days (hours and minutes say nothing there).
export function duration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000))
  if (s < 60) return t('convo.dur.s', { s })
  if (s < 3600) return t('convo.dur.ms', { m: Math.floor(s / 60), s: s % 60 })
  if (s < 48 * 3600) return t('convo.dur.hm', { h: Math.floor(s / 3600), m: Math.floor((s % 3600) / 60) })
  return t('convo.dur.d', { d: Math.floor(s / 86400) })
}

// How long a status has lasted, in words that say which (waited for you,
// at work), or how long ago it came (done, at rest), in minutes at least.
export function when(r: { since?: number; status: string }, now: number): string {
  if (!r.since || now < r.since) return ''
  const m = Math.floor((now - r.since) / 60000)
  const k = r.status === 'blocked' ? 'waitFor' : r.status === 'working' ? 'workFor' : 'ago'
  if (m < 1) return t(`convo.${k}.now`)
  if (m < 60) return t(`convo.${k}.m`, { n: m })
  if (m < 48 * 60) return t(`convo.${k}.h`, { n: Math.floor(m / 60) })
  return t(`convo.${k}.d`, { n: Math.floor(m / 1440) })
}
