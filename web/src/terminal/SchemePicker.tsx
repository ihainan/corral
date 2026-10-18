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

function Option(props: { scheme: Scheme; label?: string; hint?: string; selected: boolean; onPick: () => void }) {
  return (
    <button type="button" className="scheme-option" aria-pressed={props.selected} onClick={props.onPick}>
      <Swatch scheme={props.scheme} />
      <span className="scheme-name">
        {props.label ?? props.scheme.name}
        {props.hint && <span className="hint">{props.hint}</span>}
      </span>
      {props.selected && <Check size={16} className="scheme-check" />}
    </button>
  )
}

// Picking applies at once, so the terminal behind the dialog previews it.
export function SchemeDialog({ onClose }: { onClose: () => void }) {
  const prefs = usePrefs()
  const [q, setQ] = useState('')
  const needle = q.trim().toLowerCase()
  const match = (s: Scheme) => !needle || s.name.toLowerCase().includes(needle)
  const pick = (id: string) => setPrefs({ termTheme: id })
  const current = prefs.termTheme === 'dark' ? corralDark.id : prefs.termTheme === 'light' ? corralLight.id : prefs.termTheme
  const group = (dark: boolean) => allSchemes.filter((s) => s.dark === dark && match(s))
  const dark = group(true)
  const light = group(false)
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
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
        />
      </label>
      <div className="scheme-scroll">
        {!needle && (
          <div className="scheme-grid">
            <Option
              scheme={isDark() ? corralDark : corralLight}
              label={t('settings.termThemeUi')}
              hint={t('schemes.followHint')}
              selected={current === 'ui'}
              onPick={() => pick('ui')}
            />
          </div>
        )}
        {dark.length > 0 && <h3 className="scheme-group">{t('schemes.dark')}</h3>}
        <div className="scheme-grid">
          {dark.map((s) => <Option key={s.id} scheme={s} selected={current === s.id} onPick={() => pick(s.id)} />)}
        </div>
        {light.length > 0 && <h3 className="scheme-group">{t('schemes.light')}</h3>}
        <div className="scheme-grid">
          {light.map((s) => <Option key={s.id} scheme={s} selected={current === s.id} onPick={() => pick(s.id)} />)}
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
  const follow = usePrefs().termTheme === 'ui'
  return (
    <span className="scheme-current">
      <Swatch scheme={s} />
      <span>{follow ? t('settings.termThemeUi') : s.name}</span>
    </span>
  )
}
