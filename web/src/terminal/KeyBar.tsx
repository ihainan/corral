import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { Keyboard } from 'lucide-react'
import type { SpecialKey } from './keys'
import { t } from '../i18n'
import { builtins, isBuiltin, longOf, type KeyLayout, type LayoutKey } from './layout'

export type ModState = 'off' | 'once' | 'lock'

interface KeyProps {
  label: React.ReactNode
  aria: string
  onPress: () => void
  onLong?: () => void
  onDown?: () => void // as the press starts
  repeat?: boolean
  mod?: ModState
  wide?: boolean
  hint?: boolean // it has a long press (a mark in its corner)
  opener?: boolean // it opens the shortcut panel (a press on it is not one outside)
  describedBy?: string // what its long press does, read after its name
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
    st.current = null
  }
  // Stop any repeat if the key disappears mid-press (e.g. the Fn row toggles).
  useEffect(() => cancel, [])
  return (
    <button
      type="button"
      tabIndex={-1}
      className={'kb-key' + (p.wide ? ' wide' : '') + (p.mod && p.mod !== 'off' ? ' mod-' + p.mod : '') + (p.hint ? ' has-long' : '')}
      aria-label={p.aria}
      aria-describedby={p.describedBy}
      data-sc-opener={p.opener ? '' : undefined}
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
        cancel()
        if (s && !s.fired && !s.moved) p.onPress()
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
  layout: KeyLayout
  onMod: (m: 'ctrl' | 'alt') => void
  onFn: () => void
  onKey: (k: SpecialKey) => void
  onText: (s: string) => void
  onSteps: (keys: string) => void // a sequence (a key of one's own, a long press)
  onPanel: () => void // the shortcut panel (long press on Ctrl, or its key)
  onKeyboard: () => void
  onKeyboardDown: () => void
  panel?: ReactNode // shown above the bar
}

// Neighbouring keys of a kind (one's own, arrows, symbols) make a group;
// groups are set apart by spacing.
function groups(keys: LayoutKey[]): { at: number; keys: LayoutKey[] }[] {
  const kind = (k: LayoutKey) => (!isBuiltin(k) ? 'own' : ['up', 'down', 'left', 'right'].includes(k.k) ? 'arrow' : k.k.startsWith('sym:') ? 'sym' : k.k)
  const out: { at: number; keys: LayoutKey[] }[] = []
  keys.forEach((k, i) => {
    const last = out.at(-1)
    if (last && kind(last.keys[0]) === kind(k)) last.keys.push(k)
    else out.push({ at: i, keys: [k] })
  })
  return out
}

const fkeys: SpecialKey[] = ['F1', 'F2', 'F3', 'F4', 'F5', 'F6', 'F7', 'F8', 'F9', 'F10', 'F11', 'F12']

// Layout (the user's, see layout.ts): pinned keys stay put on the left, the
// keyboard toggle on the right; the row between scrolls sideways. Fn swaps
// the row for function and navigation keys.
export function KeyBar(p: KeyBarProps) {
  const uid = useId()
  const rowRef = useRef<HTMLDivElement>(null)
  const [atEnd, setAtEnd] = useState(false)
  const onScroll = () => {
    const el = rowRef.current
    if (el) setAtEnd(el.scrollLeft + el.clientWidth >= el.scrollWidth - 2)
  }
  useEffect(onScroll, [p.fn, p.layout])
  const key = (k: LayoutKey, i: number) => {
    const long = longOf(k)
    const onLong = long ? () => p.onSteps(long) : undefined
    if (!isBuiltin(k)) {
      return <Key key={'c' + i} label={<span className="kb-text">{k.label}</span>} aria={k.label} wide onPress={() => p.onSteps(k.keys)} onLong={onLong} hint={!!long} />
    }
    const b = builtins[k.k]
    if ('mod' in b) {
      const m = b.mod
      // (Its long press named, for those who cannot see the dot — ux1101 T08.)
      return <Key key={k.k} label={b.label} aria={b.aria} describedBy={m === 'ctrl' ? uid + '-ctrl' : undefined} mod={m === 'ctrl' ? p.ctrl : p.alt} onPress={() => p.onMod(m)} onLong={m === 'ctrl' ? p.onPanel : undefined} hint={m === 'ctrl'} opener={m === 'ctrl'} />
    }
    if ('act' in b) {
      return b.act === 'fn'
        ? <Key key={k.k} label={b.label} aria={b.aria} mod={p.fn ? 'once' : 'off'} onPress={p.onFn} />
        : <Key key={k.k} label={<span className="kb-text">{t('keybar.panel')}</span>} aria={t('sc.title')} wide onPress={p.onPanel} opener />
    }
    if ('text' in b) return <Key key={k.k} label={b.label} aria={b.aria} onPress={() => p.onText(b.text)} onLong={onLong} hint={!!long} />
    const newline = b.key === 'AgentNewline'
    const label = newline ? <span className="kb-text">{t('term.newlineKey')}</span> : b.label
    const repeat = 'repeat' in b && !!b.repeat
    return (
      <Key key={k.k} label={label} aria={newline ? t('term.agentNewline') : b.aria} wide={newline || b.label.length > 3} repeat={repeat} onPress={() => p.onKey(b.key)} onLong={repeat ? undefined : onLong} hint={!repeat && !!long} />
    )
  }
  const fnInRow = p.layout.row.find((k) => isBuiltin(k) && k.k === 'fn')
  // A hardware keyboard reaches the bar as one stop (the key last on, else
  // the first), Left and Right (Home, End) moving along it (ux1101 T18).
  // Presses never focus a key (see Key), so a tap keeps the terminal's focus.
  const barRef = useRef<HTMLDivElement>(null)
  const keysOf = () => [...(barRef.current?.querySelectorAll<HTMLButtonElement>(':scope > .kb-group .kb-key, :scope > .kb-row .kb-key') ?? [])]
  useLayoutEffect(() => {
    const keys = keysOf()
    if (keys.length && !keys.some((k) => k.tabIndex === 0)) keys[0].tabIndex = 0
  })
  const roam = (e: React.KeyboardEvent) => {
    if (e.altKey || e.ctrlKey || e.metaKey) return // (a chord is not a move along the bar)
    const keys = keysOf()
    const i = keys.indexOf(document.activeElement as HTMLButtonElement)
    if (i < 0) return
    const next = e.key === 'ArrowRight' ? Math.min(i + 1, keys.length - 1) : e.key === 'ArrowLeft' ? Math.max(i - 1, 0) : e.key === 'Home' ? 0 : e.key === 'End' ? keys.length - 1 : -1
    if (next < 0) return
    e.preventDefault()
    e.stopPropagation()
    keys[next].focus()
    keys[next].scrollIntoView({ block: 'nearest', inline: 'nearest' })
  }
  const onFocus = (e: React.FocusEvent) => {
    const keys = keysOf()
    if (!keys.includes(e.target as HTMLButtonElement)) return
    for (const k of keys) k.tabIndex = k === e.target ? 0 : -1
  }
  return (
    <div className="keybar" role="toolbar" aria-label={t('term.keys')} ref={barRef} onKeyDown={roam} onFocus={onFocus}>
      {p.panel}
      <span id={uid + '-ctrl'} className="sr-only">{t('keybar.ctrlLong')}</span>
      <span id={uid + '-kb'} className="sr-only">{t('keybar.kbLong')}</span>
      {p.layout.pinned.length > 0 && <div className="kb-group kb-fixed kb-pinned">{p.layout.pinned.map(key)}</div>}
      <div className={'kb-row' + (atEnd ? ' at-end' : '')} ref={rowRef} onScroll={onScroll}>
        {p.fn ? (
          <>
            {fnInRow && <div className="kb-group">{key(fnInRow, -1)}</div>}
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
          groups(p.layout.row).map((g) => (
            <div key={g.at} className="kb-group">
              {g.keys.map((k, i) => key(k, g.at + i))}
            </div>
          ))
        )}
      </div>
      <div className="kb-group kb-fixed">
        <Key label={<Keyboard size={18} />} aria={t('term.keyboard')} describedBy={uid + '-kb'} onPress={p.onKeyboard} onDown={p.onKeyboardDown} onLong={() => p.onKey('Enter')} hint />
      </div>
    </div>
  )
}
