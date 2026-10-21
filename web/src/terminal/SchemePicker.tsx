import { useState } from 'react'
import { Check, Search } from 'lucide-react'
import { t } from '../i18n'
import { isDark, setPrefs, termScheme, usePrefs } from '../prefs'
import { Dialog } from '../components/ui'
import { allSchemes, corralDark, corralLight, type Scheme } from './themes'

// A palette in miniature: its background with a prompt in its foreground and
// the six main ANSI colours, so schemes can be told apart at a glance.
export function Swatch({ scheme }: { scheme: Scheme }) {
  const th = scheme.theme
  return (
    <span className="swatch" style={{ background: th.background, color: th.foreground }} aria-hidden="true">
      <span className="swatch-prompt">~$</span>
      <span className="swatch-dots">
        {[th.red, th.green, th.yellow, th.blue, th.magenta, th.cyan].map((c, i) => (
          <i key={i} style={{ background: c }} />
        ))}
      </span>
    </span>
  )
}

function Option(props: { scheme: Scheme; label?: string; hint?: string; selected: boolean; onPick: () => void; tabStop: boolean }) {
  return (
    <button type="button" className="scheme-option" aria-pressed={props.selected} onClick={props.onPick} tabIndex={props.tabStop ? 0 : -1}>
      <Swatch scheme={props.scheme} />
      <span className="scheme-name">
        {props.label ?? props.scheme.name}
        {props.hint && <span className="hint">{props.hint}</span>}
      </span>
      {props.selected && <Check size={16} className="scheme-check" />}
    </button>
  )
}

// The list is one Tab stop (the selected option, else the first); arrow
// keys move by what is on screen: left/right to the neighbour, up/down to
// the nearest option in the row above or below.
function arrowNav(e: React.KeyboardEvent<HTMLElement>) {
  const opts = [...e.currentTarget.querySelectorAll<HTMLElement>('.scheme-option')]
  const cur = document.activeElement as HTMLElement
  const i = opts.indexOf(cur)
  if (i < 0) return
  let next: HTMLElement | undefined
  if (e.key === 'ArrowRight') next = opts[i + 1]
  else if (e.key === 'ArrowLeft') next = opts[i - 1]
  else if (e.key === 'Home') next = opts[0]
  else if (e.key === 'End') next = opts[opts.length - 1]
  else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    const r = cur.getBoundingClientRect()
    const down = e.key === 'ArrowDown'
    const rows = opts.filter((o) => {
      const t = o.getBoundingClientRect().top
      return down ? t > r.top + 4 : t < r.top - 4
    })
    if (rows.length) {
      const rowTop = down ? Math.min(...rows.map((o) => o.getBoundingClientRect().top)) : Math.max(...rows.map((o) => o.getBoundingClientRect().top))
      const row = rows.filter((o) => Math.abs(o.getBoundingClientRect().top - rowTop) < 4)
      next = row.reduce((a, b) => (Math.abs(b.getBoundingClientRect().left - r.left) < Math.abs(a.getBoundingClientRect().left - r.left) ? b : a))
    }
  }
  if (!e.key.startsWith('Arrow') && e.key !== 'Home' && e.key !== 'End') return
  e.preventDefault()
  if (!next) return
  for (const o of opts) o.tabIndex = o === next ? 0 : -1
  next.focus()
  next.scrollIntoView({ block: 'nearest' })
}

// Picking applies at once, so the terminal behind the dialog previews it.
export function SchemeDialog({ onClose }: { onClose: () => void }) {
  const prefs = usePrefs()
  const [q, setQ] = useState('')
  const needle = q.trim().toLowerCase()
  const match = (s: Scheme) => !needle || s.name.toLowerCase().includes(needle)
  // Following the page: a scheme picked is the one for the appearance chosen
  // above the lists (at first the page's own now).
  const follow = prefs.termTheme === 'ui'
  const [slot, setSlot] = useState<'dark' | 'light'>(() => (isDark() ? 'dark' : 'light'))
  const pick = (id: string) => setPrefs(follow ? (slot === 'dark' ? { termThemeDark: id } : { termThemeLight: id }) : { termTheme: id })
  const own = follow ? (slot === 'dark' ? prefs.termThemeDark : prefs.termThemeLight) : prefs.termTheme
  const current = own === 'dark' ? corralDark.id : own === 'light' ? corralLight.id : own
  const nameOf = (id: string) => allSchemes.find((x) => x.id === id)?.name ?? id
  const group = (dark: boolean) => allSchemes.filter((s) => s.dark === dark && match(s))
  const dark = group(true)
  const light = group(false)
  const shown = [...(needle ? [] : ['ui']), ...dark.map((x) => x.id), ...light.map((x) => x.id)]
  const tabStop = shown.includes(current) ? current : shown[0]
  return (
    <Dialog title={t('settings.termTheme')} onClose={onClose} wide seeThrough>
      <label className="scheme-search">
        <Search size={16} aria-hidden="true" />
        <input
          className="input"
          type="search"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder={t('schemes.search', { n: allSchemes.length })}
          aria-label={t('schemes.searchLabel')}
          data-touch-nofocus
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
        />
      </label>
      <p className="sr-only" aria-live="polite">{needle ? t('schemes.count', { n: dark.length + light.length }) : ''}</p>
      <div className="scheme-scroll" onKeyDown={arrowNav}>
        {!needle && (
          <div className="scheme-grid">
            <Option
              scheme={termScheme()}
              label={t('settings.termThemeUi')}
              hint={t(follow ? 'schemes.followOffHint' : 'schemes.followHint')}
              selected={follow}
              tabStop={tabStop === 'ui'}
              // (Again: back to one scheme, the one shown now.)
              onPick={() => setPrefs({ termTheme: follow ? termScheme().id : 'ui' })}
            />
          </div>
        )}
        {follow && (
          <div className="seg scheme-slot" role="group" aria-label={t('schemes.slotLabel')}>
            {(['dark', 'light'] as const).map((k) => (
              <button key={k} type="button" aria-pressed={slot === k} onClick={() => setSlot(k)}>
                {t(k === 'dark' ? 'schemes.forDark' : 'schemes.forLight', { name: nameOf(k === 'dark' ? prefs.termThemeDark : prefs.termThemeLight) })}
              </button>
            ))}
          </div>
        )}
        {dark.length > 0 && <h3 className="scheme-group">{t('schemes.dark')}</h3>}
        <div className="scheme-grid">
          {dark.map((s) => <Option key={s.id} scheme={s} selected={current === s.id} tabStop={tabStop === s.id} onPick={() => pick(s.id)} />)}
        </div>
        {light.length > 0 && <h3 className="scheme-group">{t('schemes.light')}</h3>}
        <div className="scheme-grid">
          {light.map((s) => <Option key={s.id} scheme={s} selected={current === s.id} tabStop={tabStop === s.id} onPick={() => pick(s.id)} />)}
        </div>
        {dark.length + light.length === 0 && <p className="hint">{t('schemes.none')}</p>}
      </div>
      <div className="dialog-actions">
        <button className="btn btn-primary" onClick={onClose}>{t('schemes.done')}</button>
      </div>
    </Dialog>
  )
}

// The current choice as shown in Settings.
export function CurrentScheme() {
  usePrefs()
  const s = termScheme()
  const p = usePrefs()
  const follow = p.termTheme === 'ui'
  const nameOf = (id: string) => allSchemes.find((x) => x.id === id)?.name ?? id
  return (
    <span className="scheme-current">
      <Swatch scheme={s} />
      <span>{follow ? t('schemes.followPair', { dark: nameOf(p.termThemeDark), light: nameOf(p.termThemeLight) }) : s.name}</span>
    </span>
  )
}
