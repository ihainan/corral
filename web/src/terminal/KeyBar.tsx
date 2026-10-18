import { useRef } from 'react'
import { Keyboard } from 'lucide-react'
import type { SpecialKey } from './keys'
import { t } from '../i18n'

export type ModState = 'off' | 'once' | 'lock'

interface KeyProps {
  label: React.ReactNode
  aria: string
  onPress: () => void
  onLong?: () => void
  repeat?: boolean
  mod?: ModState
  wide?: boolean
  accent?: boolean
}

const MOVE_CANCEL = 10 // px: beyond this the gesture is a scroll of the bar, not a key press
const LONG_PRESS = 450
const REPEAT_DELAY = 380
const REPEAT_EVERY = 55

// A key that never takes focus from the terminal, so the soft keyboard stays up.
function Key(p: KeyProps) {
  const st = useRef<{ x: number; y: number; long?: number; rep?: number; fired: boolean; moved: boolean } | null>(null)
  const clear = () => {
    const s = st.current
    if (!s) return
    clearTimeout(s.long)
    clearTimeout(s.rep)
    clearInterval(s.rep)
  }
  return (
    <button
      type="button"
      tabIndex={-1}
      className={'kb-key' + (p.wide ? ' wide' : '') + (p.accent ? ' accent' : '') + (p.mod && p.mod !== 'off' ? ' mod-' + p.mod : '')}
      aria-label={p.aria}
      aria-pressed={p.mod ? p.mod !== 'off' : undefined}
      // Preventing mousedown (not pointerdown, which WebKit treats as cancelling
      // the click) keeps focus in the terminal's hidden input.
      onMouseDown={(e) => e.preventDefault()}
      onContextMenu={(e) => e.preventDefault()}
      onPointerDown={(e) => {
        clear()
        const s = { x: e.clientX, y: e.clientY, fired: false, moved: false } as NonNullable<typeof st.current>
        st.current = s
        if (p.onLong) {
          s.long = window.setTimeout(() => {
            s.fired = true
            navigator.vibrate?.(8)
            p.onLong!()
          }, LONG_PRESS)
        } else if (p.repeat) {
          s.rep = window.setTimeout(() => {
            s.fired = true
            p.onPress()
            s.rep = window.setInterval(p.onPress, REPEAT_EVERY)
          }, REPEAT_DELAY)
        }
      }}
      onPointerMove={(e) => {
        const s = st.current
        if (!s || s.moved) return
        if (Math.abs(e.clientX - s.x) > MOVE_CANCEL || Math.abs(e.clientY - s.y) > MOVE_CANCEL) {
          s.moved = true
          clear()
        }
      }}
      onPointerUp={() => {
        const s = st.current
        clear()
        st.current = null
        if (s && !s.fired && !s.moved) p.onPress()
      }}
      onPointerCancel={() => {
        clear()
        st.current = null
      }}
    >
      {p.label}
    </button>
  )
}

export interface KeyBarProps {
  ctrl: ModState
  alt: ModState
  fn: boolean
  onMod: (m: 'ctrl' | 'alt') => void
  onFn: () => void
  onKey: (k: SpecialKey) => void
  onText: (s: string) => void
  onKeyboard: () => void
}

export function KeyBar(p: KeyBarProps) {
  const arrows: [SpecialKey, string, string][] = [
    ['Up', '↑', 'Up'], ['Down', '↓', 'Down'], ['Left', '←', 'Left'], ['Right', '→', 'Right'],
  ]
  const symbols = ['|', '/', '~', '-', '`', '\\', '_', ':', '*', '&', '$']
  const fkeys: SpecialKey[] = ['F1', 'F2', 'F3', 'F4', 'F5', 'F6', 'F7', 'F8', 'F9', 'F10', 'F11', 'F12']
  return (
    <div className="keybar" role="toolbar" aria-label="Keys">
      <div className="kb-row">
        <Key label="Esc" aria="Escape" onPress={() => p.onKey('Esc')} onLong={() => { p.onKey('Esc'); p.onKey('Esc') }} />
        <Key label="Ctrl" aria="Ctrl" mod={p.ctrl} onPress={() => p.onMod('ctrl')} />
        <Key label="Tab" aria="Tab" onPress={() => p.onKey('Tab')} onLong={() => p.onKey('ShiftTab')} />
        {p.fn ? (
          <>
            {fkeys.map((k) => <Key key={k} label={k} aria={k} onPress={() => p.onKey(k)} />)}
            <Key label="Home" aria="Home" wide onPress={() => p.onKey('Home')} />
            <Key label="End" aria="End" wide onPress={() => p.onKey('End')} />
            <Key label="PgUp" aria="Page Up" wide repeat onPress={() => p.onKey('PageUp')} />
            <Key label="PgDn" aria="Page Down" wide repeat onPress={() => p.onKey('PageDown')} />
            <Key label="Del" aria="Delete" onPress={() => p.onKey('Delete')} />
            <Key label="Alt" aria="Alt" mod={p.alt} onPress={() => p.onMod('alt')} />
          </>
        ) : (
          <>
            {arrows.map(([k, label, aria]) => <Key key={k} label={label} aria={aria} repeat onPress={() => p.onKey(k)} />)}
            <Key label="Alt" aria="Alt" mod={p.alt} onPress={() => p.onMod('alt')} />
            <Key label="⏎" aria={t('term.agentNewline')} onPress={() => p.onKey('AgentNewline')} />
            {symbols.map((s) => <Key key={s} label={s} aria={s} onPress={() => p.onText(s)} />)}
          </>
        )}
      </div>
      <div className="kb-fixed">
        <Key label="Fn" aria="Fn" accent={p.fn} onPress={p.onFn} />
        <Key label={<Keyboard size={18} />} aria={t('term.keyboard')} onPress={p.onKeyboard} onLong={() => p.onKey('Enter')} />
      </div>
    </div>
  )
}
