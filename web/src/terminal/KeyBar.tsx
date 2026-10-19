import { useEffect, useRef, useState } from 'react'
import { Keyboard } from 'lucide-react'
import type { SpecialKey } from './keys'
import { t } from '../i18n'

export type ModState = 'off' | 'once' | 'lock'

interface KeyProps {
  label: React.ReactNode
  aria: string
  onPress: () => void
  onLong?: () => void
  onDown?: () => void // as the press starts
  onEnd?: () => void // as it ends, after onPress if it was a press
  repeat?: boolean
  mod?: ModState
  wide?: boolean
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
  const cancel = () => {
    clear()
    const was = st.current
    st.current = null
    if (was) p.onEnd?.()
  }
  // Stop any repeat if the key disappears mid-press (e.g. the Fn row toggles).
  useEffect(() => cancel, [])
  return (
    <button
      type="button"
      tabIndex={-1}
      className={'kb-key' + (p.wide ? ' wide' : '') + (p.mod && p.mod !== 'off' ? ' mod-' + p.mod : '')}
      aria-label={p.aria}
      aria-pressed={p.mod ? p.mod !== 'off' : undefined}
      // Preventing mousedown (not pointerdown, which WebKit treats as cancelling
      // the click) keeps focus in the terminal's hidden input.
      onMouseDown={(e) => e.preventDefault()}
      onContextMenu={(e) => e.preventDefault()}
      // Activation without a pointer (VoiceOver, Switch Control, keyboard) arrives
      // as a click with detail 0; pointer presses are handled below.
      onClick={(e) => {
        if (e.detail === 0) p.onPress()
      }}
      onPointerDown={(e) => {
        clear()
        p.onDown?.()
        e.currentTarget.setPointerCapture?.(e.pointerId)
        const s = { x: e.clientX, y: e.clientY, fired: false, moved: false } as NonNullable<typeof st.current>
        st.current = s
        if (p.onLong) {
          s.long = window.setTimeout(() => {
            s.fired = true
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
        if (s) p.onEnd?.()
      }}
      onPointerCancel={cancel}
      onLostPointerCapture={cancel}
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
  onKeyboardDown: () => void
  onKeyboardEnd: () => void
  custom: { label: string; onPress: () => void }[] // the user's own keys
}

// Layout: Esc/Tab and the modifiers stay put on the left, the keyboard toggle
// on the right; everything between scrolls sideways. Fn swaps the scrolling
// part for function and navigation keys.
export function KeyBar(p: KeyBarProps) {
  const arrows: [SpecialKey, string, string][] = [
    ['Up', '↑', 'Up'], ['Down', '↓', 'Down'], ['Left', '←', 'Left'], ['Right', '→', 'Right'],
  ]
  const symbols = ['|', '/', '~', '-', '`', '\\', '_', ':', '*', '&', '$']
  const fkeys: SpecialKey[] = ['F1', 'F2', 'F3', 'F4', 'F5', 'F6', 'F7', 'F8', 'F9', 'F10', 'F11', 'F12']
  const rowRef = useRef<HTMLDivElement>(null)
  const [atEnd, setAtEnd] = useState(false)
  const onScroll = () => {
    const el = rowRef.current
    if (el) setAtEnd(el.scrollLeft + el.clientWidth >= el.scrollWidth - 2)
  }
  useEffect(onScroll, [p.fn])
  return (
    <div className="keybar" role="toolbar" aria-label={t('term.keys')}>
      <div className="kb-group kb-fixed">
        <Key label="Esc" aria="Escape" onPress={() => p.onKey('Esc')} onLong={() => { p.onKey('Esc'); p.onKey('Esc') }} />
        <Key label="Tab" aria="Tab" onPress={() => p.onKey('Tab')} onLong={() => p.onKey('ShiftTab')} />
        <Key label="Ctrl" aria="Ctrl" mod={p.ctrl} onPress={() => p.onMod('ctrl')} />
        <Key label="Alt" aria="Alt" mod={p.alt} onPress={() => p.onMod('alt')} />
      </div>
      <div className={'kb-row' + (atEnd ? ' at-end' : '')} ref={rowRef} onScroll={onScroll}>
        <div className="kb-group">
          <Key label="Fn" aria="Fn" mod={p.fn ? 'once' : 'off'} onPress={p.onFn} />
        </div>
        {!p.fn && p.custom.length > 0 && (
          <div className="kb-group">
            {p.custom.map((k, i) => (
              <Key key={i} label={<span className="kb-text">{k.label}</span>} aria={k.label} wide onPress={k.onPress} />
            ))}
          </div>
        )}
        {p.fn ? (
          <>
            <div className="kb-group">
              {fkeys.map((k) => <Key key={k} label={k} aria={k} onPress={() => p.onKey(k)} />)}
            </div>
            <div className="kb-group">
              <Key label="Home" aria="Home" wide onPress={() => p.onKey('Home')} />
              <Key label="End" aria="End" wide onPress={() => p.onKey('End')} />
              <Key label="PgUp" aria="Page Up" wide repeat onPress={() => p.onKey('PageUp')} />
              <Key label="PgDn" aria="Page Down" wide repeat onPress={() => p.onKey('PageDown')} />
              <Key label="Del" aria="Delete" onPress={() => p.onKey('Delete')} />
            </div>
          </>
        ) : (
          <>
            <div className="kb-group">
              {arrows.map(([k, label, aria]) => <Key key={k} label={label} aria={aria} repeat onPress={() => p.onKey(k)} />)}
            </div>
            <div className="kb-group">
              <Key label={<span className="kb-text">{t('term.newlineKey')}</span>} aria={t('term.agentNewline')} wide onPress={() => p.onKey('AgentNewline')} />
            </div>
            <div className="kb-group">
              {symbols.map((s) => <Key key={s} label={s} aria={s} onPress={() => p.onText(s)} />)}
            </div>
          </>
        )}
      </div>
      <div className="kb-group kb-fixed">
        <Key label={<Keyboard size={18} />} aria={t('term.keyboard')} onPress={p.onKeyboard} onDown={p.onKeyboardDown} onEnd={p.onKeyboardEnd} onLong={() => p.onKey('Enter')} />
      </div>
    </div>
  )
}
