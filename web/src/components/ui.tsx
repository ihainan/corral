import { cloneElement, createContext, isValidElement, useCallback, useContext, useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { Check, CircleAlert, CircleCheck, CircleHelp, Copy, Info, TriangleAlert, X } from 'lucide-react'
import { errorText } from '../api'
import { t } from '../i18n'

export const isCoarse = () => window.matchMedia('(pointer: coarse)').matches

// Whether a media query matches, kept up to date (a window resized, a tablet turned).
export function useMedia(query: string): boolean {
  const [on, setOn] = useState(() => window.matchMedia(query).matches)
  useEffect(() => {
    const m = window.matchMedia(query)
    const f = () => setOn(m.matches)
    f()
    m.addEventListener('change', f)
    return () => m.removeEventListener('change', f)
  }, [query])
  return on
}

// Whether the latest interaction was a key press (a hardware keyboard) rather
// than a tap or click.
let lastWasKey = false
if (typeof document !== 'undefined') {
  document.addEventListener('keydown', () => (lastWasKey = true), true)
  document.addEventListener('pointerdown', () => (lastWasKey = false), true)
}
// Whether the latest input was a key rather than a press (a hardware keyboard in use).
export const keysInUse = () => lastWasKey
const isTextEntry = (el: Element | null) => !!el && (el.tagName === 'TEXTAREA' || el.tagName === 'INPUT' || (el as HTMLElement).isContentEditable)

// A layer opened by a tap on a touch screen puts the on-screen keyboard
// away, and closing it does not bring the keyboard back: focus returns to a
// text field only when keys are in use. leaveField runs on open, restoreFocus
// on close.
export function leaveField() {
  const a = document.activeElement
  if (isCoarse() && !lastWasKey && isTextEntry(a)) (a as HTMLElement).blur()
}
export function restoreFocus(prev: HTMLElement | null, root: HTMLElement | null) {
  // Give focus back only if it was ours to give: a layer that closes
  // underneath another must not pull focus out of the one on top.
  const a = document.activeElement
  if (a && a !== document.body && !root?.contains(a)) return
  if (isCoarse() && !lastWasKey && isTextEntry(prev)) {
    if (a && a !== document.body) (a as HTMLElement).blur()
    return
  }
  prev?.focus?.()
}

// Open layers, innermost last. Only the top one reacts to Tab and Escape, so a
// dialog opened over a sheet does not fight it for focus.
const layers: object[] = []
function useLayer() {
  const id = useRef({}).current
  useEffect(() => {
    layers.push(id)
    return () => {
      const i = layers.lastIndexOf(id)
      if (i >= 0) layers.splice(i, 1)
    }
  }, [id])
  return useCallback(() => layers[layers.length - 1] === id, [id])
}

// Focus handling for modal layers: focus moves in on open, Tab stays inside,
// Escape closes, and focus returns to where it was afterwards.
export function useFocusTrap<T extends HTMLElement>(onClose: () => void) {
  const ref = useRef<T>(null)
  const closeRef = useRef(onClose)
  closeRef.current = onClose
  const isTop = useLayer()
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    const root = ref.current
    leaveField()
    const focusables = () =>
      [...(root?.querySelectorAll<HTMLElement>('a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])') ?? [])]
        .filter((el) => el.offsetParent !== null || el === document.activeElement)
    // Fields marked data-touch-nofocus are skipped on touch screens, where
    // focusing them would pop the keyboard over the content.
    const skip = isCoarse() ? ':not([data-touch-nofocus])' : ''
    // (Not a dialog's close in its corner: Enter there would shut it — 11-14.)
    const first = root?.querySelector<HTMLElement>(`input${skip}, textarea${skip}, select${skip}`) ?? focusables().find((el) => !el.classList.contains('dialog-x') && !(isCoarse() && el.hasAttribute('data-touch-nofocus')))
    first?.focus()
    const onKey = (e: KeyboardEvent) => {
      if (!isTop()) return
      if (e.key === 'Escape') {
        e.stopPropagation()
        closeRef.current()
        return
      }
      if (e.key !== 'Tab') return
      const list = focusables()
      if (list.length === 0) {
        e.preventDefault()
        return
      }
      const i = list.indexOf(document.activeElement as HTMLElement)
      if (e.shiftKey && i <= 0) {
        e.preventDefault()
        list[list.length - 1].focus()
      } else if (!e.shiftKey && (i === -1 || i === list.length - 1)) {
        e.preventDefault()
        list[0].focus()
      }
    }
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('keydown', onKey, true)
      restoreFocus(prev, root)
    }
  }, [])
  return ref
}

// Dialog ---------------------------------------------------------------------

// seeThrough: a light backdrop, for dialogs whose effect shows behind them.
// guard: while true (something typed in it not yet saved, a request under
// way, words shown only once), a press outside it or Escape does not close
// it — only its own buttons do (ux1101 PG-03/PG-04).
// icon, tone, sub: its head (11-14) — what it is for, on a tinted square (red
// for what cannot be undone, amber for a warning), a line under the title.
// The close in its corner is its cancel: off while busy, as its cancel is.
// Prevent a pointerdown that closes a floating popover/sheet from letting the
// synthesized click land on whatever interactive element sat underneath it.
export function swallowNextClick() {
  const stop = (e: MouseEvent) => {
    e.preventDefault()
    e.stopPropagation()
    cleanup()
  }
  const timer = window.setTimeout(cleanup, 400)
  function cleanup() {
    window.clearTimeout(timer)
    document.removeEventListener('click', stop, true)
  }
  document.addEventListener('click', stop, true)
}

let openDialogs = 0

export function Dialog(props: { title: string; onClose: () => void; children: ReactNode; sheet?: boolean; hideTitle?: boolean; wide?: boolean; seeThrough?: boolean; guard?: boolean; busy?: boolean; icon?: ReactNode; tone?: 'danger' | 'warn'; sub?: ReactNode; className?: string; dialogProps?: React.HTMLAttributes<HTMLDivElement> }) {
  const dismiss = () => !props.guard && !props.busy && props.onClose()
  const ref = useFocusTrap<HTMLDivElement>(dismiss)
  const backDown = useRef(false)
  useEffect(() => {
    if (++openDialogs === 1) document.documentElement.classList.add('modal-open')
    return () => {
      if (--openDialogs <= 0) {
        openDialogs = 0
        document.documentElement.classList.remove('modal-open')
      }
    }
  }, [])
  useSheetDrag(ref, props.sheet !== false && !props.guard && !props.busy, props.onClose)
  return createPortal(
    <div
      className={'overlay' + (props.sheet !== false ? ' sheet-mode' : '') + (props.seeThrough ? ' see-through' : '')}
      onPointerDown={(e) => {
        backDown.current = e.target === e.currentTarget
      }}
      onClick={(e) => {
        const hit = backDown.current && e.target === e.currentTarget
        backDown.current = false
        if (!hit) return
        e.preventDefault()
        e.stopPropagation()
        dismiss()
      }}
    >
      <div {...props.dialogProps} className={'dialog' + (props.wide ? ' dialog-wide' : '') + (props.className ? ' ' + props.className : '')} role="dialog" aria-modal="true" aria-label={props.title} ref={ref}>
        {props.hideTitle ? null : (
          <>
            {/* (First, so the actions stay the last child; never the one focused on opening — useFocusTrap.) */}
            <button type="button" className="icon-btn dialog-x" onClick={props.onClose} disabled={props.busy} aria-label={t('common.close')} title={t('common.close')}>
              <X size={18} />
            </button>
            {props.icon || props.sub ? (
              <div className="dialog-head">
                {props.icon && <span className={'dialog-icon' + (props.tone ? ' is-' + props.tone : '')} aria-hidden="true">{props.icon}</span>}
                <div className="dialog-head-text">
                  <h2>{props.title}</h2>
                  {props.sub && <p className="dialog-sub">{props.sub}</p>}
                </div>
              </div>
            ) : (
              <h2>{props.title}</h2>
            )}
          </>
        )}
        {props.children}
      </div>
    </div>,
    document.body,
  )
}

// A bottom sheet pulled down closes (11-18: the bar on its top said it
// could be, and nothing came of it). Only where it is drawn as a sheet
// (narrow, app.css), not while it may not be closed (guard, busy); a pull
// is taken only from where nothing above the finger is scrolled down, in
// a field nothing, and only downwards and more down than across — else
// the sheet's own scrolling (or a wide screen's, sideways) is left alone.
// Let go past a fifth of it, or flicked down, it goes; else it springs back.
const SHEET_QUERY = '(max-width: 520px)'
function useSheetDrag(ref: React.RefObject<HTMLDivElement | null>, on: boolean, onClose: () => void) {
  const closeRef = useRef(onClose)
  closeRef.current = onClose
  useEffect(() => {
    const el = ref.current
    if (!on || !el) return
    let y0 = 0, x0 = 0, dy = 0, state: 'idle' | 'maybe' | 'drag' | 'off' = 'idle'
    // (Where the finger was lately: how fast it moved as it let go, not on the whole.)
    let trail: { y: number; t: number }[] = []
    let timer = 0 // (the close after its slide; dropped if the hook ends first)
    const back = () => {
      el.style.transition = window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'none' : 'transform 0.18s ease-out'
      el.style.transform = ''
    }
    const scrolledAbove = (n: Node | null) => {
      for (let e = n instanceof Element ? n : n?.parentElement ?? null; e && e !== el.parentElement; e = e.parentElement) {
        if (e.scrollTop > 0) return true
        if (e === el) break
      }
      return false
    }
    const start = (ev: TouchEvent) => {
      // (A second finger mid-pull: the pull let go of, the sheet back.)
      if (state === 'drag') back()
      state = 'off'
      if (ev.touches.length !== 1 || !window.matchMedia(SHEET_QUERY).matches) return
      const tgt = ev.target as Element | null
      if (tgt?.closest('input, textarea, select, [contenteditable="true"], .convo-panelwin-screen') || scrolledAbove(tgt)) return
      y0 = ev.touches[0].clientY; x0 = ev.touches[0].clientX; dy = 0
      trail = [{ y: y0, t: ev.timeStamp }]
      state = 'maybe'
    }
    const move = (ev: TouchEvent) => {
      if (state === 'off' || state === 'idle') return
      const d = ev.touches[0].clientY - y0, dx = Math.abs(ev.touches[0].clientX - x0)
      if (state === 'maybe') {
        if (Math.abs(d) < 6 && dx < 6) return
        if (d <= 0 || dx > d) { state = 'off'; return } // (up, or across: the sheet's own)
        state = 'drag'
        el.style.transition = 'none'
      }
      ev.preventDefault() // (the page under it not pulled too)
      dy = Math.max(0, d)
      trail.push({ y: ev.touches[0].clientY, t: ev.timeStamp })
      while (trail.length > 2 && ev.timeStamp - trail[1].t > 100) trail.shift()
      el.style.transform = `translateY(${dy}px)`
    }
    const end = (ev?: TouchEvent) => {
      if (state !== 'drag') { state = 'idle'; return }
      state = 'idle'
      const a = trail[0], b = trail[trail.length - 1]
      // (Moving as it let go: held still after, it was no flick.)
      const fast = !!ev && dy > 24 && ev.timeStamp - b.t < 60 && (b.y - a.y) / Math.max(1, b.t - a.t) > 0.5 // (px/ms)
      if (dy > el.offsetHeight / 5 || fast) {
        if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return closeRef.current()
        el.style.transition = 'transform 0.16s ease-in'
        el.style.transform = `translateY(${el.offsetHeight}px)`
        timer = window.setTimeout(() => { timer = 0; closeRef.current() }, 160)
      } else back()
    }
    el.addEventListener('touchstart', start, { passive: true })
    el.addEventListener('touchmove', move, { passive: false })
    el.addEventListener('touchend', end)
    const cancel = () => { dy = 0; end() } // (taken from it: back, not closed)
    el.addEventListener('touchcancel', cancel)
    return () => {
      el.removeEventListener('touchstart', start)
      el.removeEventListener('touchmove', move)
      el.removeEventListener('touchend', end)
      el.removeEventListener('touchcancel', cancel)
      // (Unmounted, or no longer to be closed — guard, busy — in its slide: not closed now.)
      window.clearTimeout(timer)
      el.style.transform = ''
      el.style.transition = ''
    }
  }, [ref, on])
}

// The head of a dialog whose body is a thing to look at (a file, a picture,
// a command): what it is, a line under it (where it is, how big, how it
// goes), the actions on it as icons, and the close — one row, the same in
// each (app.css, .dlg-*). Its Dialog takes className "dlg" and hideTitle.
// tile: its icon on a tinted square, as a form dialog's head (Dialog's icon).
export function DialogHead(props: { title: ReactNode; titleText?: string; oneLine?: boolean; clamp?: boolean; eyebrow?: ReactNode; sub?: ReactNode; subText?: string; icon?: ReactNode; tile?: boolean; tools?: ReactNode; onClose: () => void }) {
  return (
    <div className="dlg-head">
      {props.icon && <span className={'dlg-icon' + (props.tile ? ' is-tile' : '')} aria-hidden="true">{props.icon}</span>}
      <div className="dlg-head-text">
        {props.eyebrow && <span className="dlg-eyebrow">{props.eyebrow}</span>}
        <h2 className={'dlg-title' + (props.oneLine ? ' is-one' : props.clamp ? ' is-clamped' : '')} title={props.titleText}>{props.title}</h2>
        {props.sub && <span className="dlg-sub" title={props.subText}>{props.sub}</span>}
      </div>
      {props.tools && <div className="dlg-tools">{props.tools}</div>}
      <button type="button" className="icon-btn dlg-close" onClick={props.onClose} aria-label={t('common.close')} title={t('common.close')}>
        <X size={18} />
      </button>
    </div>
  )
}

// An action of a dialog's head: an icon, its name said on hover and to the
// screen reader.
export function HeadTool(props: { label: string; onClick: () => void; children: ReactNode; pressed?: boolean }) {
  return (
    <button type="button" className="icon-btn" onClick={props.onClick} aria-label={props.label} title={props.label} aria-pressed={props.pressed}>
      {props.children}
    </button>
  )
}

export function Confirm(props: { title: string; body: string; confirm: string; danger?: boolean; onConfirm: () => Promise<void> | void; onClose: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // Once confirmed, the request cannot be called back: the dialog stays
  // until it is answered (closing it would look like cancelling).
  const close = () => !busy && props.onClose()
  return (
    <Dialog title={props.title} onClose={close} busy={busy} icon={props.danger ? <TriangleAlert size={18} /> : <CircleHelp size={18} />} tone={props.danger ? 'danger' : undefined}>
      <p className="lead">{props.body}</p>
      {error && <p className="err" role="alert">{error}</p>}
      <div className="dialog-actions">
        <button className="btn" onClick={close} disabled={busy}>{t('common.cancel')}</button>
        <button
          className={'btn ' + (props.danger ? 'btn-danger' : 'btn-primary')}
          disabled={busy}
          onClick={async () => {
            setBusy(true)
            try {
              await props.onConfirm()
              props.onClose()
            } catch (e: any) {
              setError(typeof e?.human === 'string' ? e.human : errorText(e)) // (its own words where it has them; never a network error's English, "TypeError: …": ux1101 PG-31)
              setBusy(false)
            }
          }}
        >
          {props.confirm}
        </button>
      </div>
    </Dialog>
  )
}

// Alert ----------------------------------------------------------------------
// Desktop-native centered alert dialog: app icon, centered title (receives
// focus on open and on view changes so Enter does not trigger the default
// action), scrollable body, equal-width wrapping buttons pinned at the bottom.
// Stays centered on phones (never becomes a bottom sheet).

export interface AlertAction {
  label: string
  onClick: () => void
  tone?: 'primary' | 'danger' | 'default'
  disabled?: boolean
}

export function Alert(props: {
  title: string
  viewKey?: string | number
  icon?: boolean
  onClose: () => void
  canClose?: boolean
  children?: ReactNode
  actions?: AlertAction[]
  live?: string
  className?: string
}) {
  const titleId = useId()
  const boxRef = useRef<HTMLDivElement>(null)
  const titleRef = useRef<HTMLHeadingElement>(null)
  const backDown = useRef(false)
  const canClose = props.canClose !== false
  const closeRef = useRef(() => {
    if (canClose) props.onClose()
  })
  closeRef.current = () => {
    if (canClose) props.onClose()
  }
  const isTop = useLayer()

  useEffect(() => {
    if (++openDialogs === 1) document.documentElement.classList.add('modal-open')
    return () => {
      if (--openDialogs <= 0) {
        openDialogs = 0
        document.documentElement.classList.remove('modal-open')
      }
    }
  }, [])

  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    const root = boxRef.current
    leaveField()
    titleRef.current?.focus()
    const focusables = () =>
      [...(root?.querySelectorAll<HTMLElement>('a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])') ?? [])]
        .filter((el) => el.offsetParent !== null || el === document.activeElement)
    const onKey = (e: KeyboardEvent) => {
      if (!isTop()) return
      if (e.key === 'Escape') {
        e.stopPropagation()
        e.preventDefault()
        closeRef.current()
        return
      }
      if (e.key !== 'Tab') return
      const list = focusables()
      if (list.length === 0) {
        e.preventDefault()
        return
      }
      const active = document.activeElement as HTMLElement | null
      const i = active ? list.indexOf(active) : -1
      if (e.shiftKey) {
        if (i <= 0) {
          e.preventDefault()
          list[list.length - 1].focus()
        }
      } else if (i === -1 || i === list.length - 1) {
        e.preventDefault()
        list[0].focus()
      }
    }
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('keydown', onKey, true)
      restoreFocus(prev, root)
    }
  }, [isTop])

  // Move focus to the new view's heading when switching steps inside the alert.
  const firstView = useRef(true)
  useEffect(() => {
    if (firstView.current) {
      firstView.current = false
      return
    }
    titleRef.current?.focus()
  }, [props.viewKey])

  return createPortal(
    <div
      className="alert-overlay"
      onPointerDown={(e) => {
        backDown.current = e.target === e.currentTarget
      }}
      onClick={(e) => {
        const hit = backDown.current && e.target === e.currentTarget
        backDown.current = false
        if (!hit) return
        e.preventDefault()
        e.stopPropagation()
        if (canClose) props.onClose()
      }}
    >
      <div
        ref={boxRef}
        className={'alert-box' + (props.className ? ' ' + props.className : '')}
        role="alertdialog"
        aria-modal="true"
        aria-labelledby={titleId}
      >
        <div className="alert-scroll">
          {props.icon !== false && (
            <img className="alert-icon" src="/icons/icon.svg" alt="" width={48} height={48} aria-hidden="true" />
          )}
          <h4 ref={titleRef} id={titleId} className="alert-title" tabIndex={-1}>
            {props.title}
          </h4>
          {props.children}
        </div>
        {props.actions && props.actions.length > 0 && (
          <div className="alert-actions">
            {props.actions.map((a, idx) => (
              <button
                key={idx}
                type="button"
                className={
                  'alert-btn' +
                  (a.tone === 'primary' ? ' is-primary' : a.tone === 'danger' ? ' is-danger' : '')
                }
                disabled={a.disabled}
                onClick={a.onClick}
              >
                {a.label}
              </button>
            ))}
          </div>
        )}
        {props.live !== undefined && (
          <span className="sr-only" aria-live="polite" aria-atomic="true">
            {props.live}
          </span>
        )}
      </div>
    </div>,
    document.body,
  )
}

// Menu: a popover on desktop, a bottom sheet on touch screens. Rendered in a
// portal so lists and scroll containers never clip it.

export interface MenuItem {
  label: string
  icon?: ReactNode
  onSelect?: () => void
  href?: string
  danger?: boolean
  separator?: boolean // draw a divider before this item
  keepOpen?: boolean // e.g. "retry": the menu stays open to show the result
  disabled?: boolean // shown, not to be picked now (title: why)
  title?: string
  checked?: boolean // an on/off one (a menuitemcheckbox): on or not
  hint?: string // a line under it, where all can read it (a touch screen shows no title): why it is off, say
  keys?: string[] // the page's keys that do the same, each in a box at its end (not on a touch screen: no keys there)
  trail?: ReactNode // trailing adornment at the right edge (e.g. active checkmark alongside a leading icon)
  group?: string // a small heading over it: the first of a group of items of one kind
  // Inline action buttons on the right of a single menu row (e.g. pinned order: top / up / down).
  // (text: a word instead of an icon, as a segmented control's; on: the one chosen now, of a choice of several — a
  // menuitemradio, checked or not.)
  actions?: { label: string; icon?: ReactNode; text?: string; on?: boolean; disabled?: boolean; onSelect: () => void }[]
}

// Sent on the window to close every open menu (a terminal tab sent to the back).
export const CLOSE_MENUS = 'corral:close-menus'

export function Menu(props: { label: string; heading?: string; button: (open: boolean, toggle: (e: React.MouseEvent<HTMLElement>) => void) => ReactNode; items: () => (MenuItem | null)[]; note?: string; onOpenChange?: (open: boolean) => void }) {
  const [anchor, setAnchor] = useState<DOMRect | null>(null)
  const [sheet, setSheet] = useState(false)
  const trigger = useRef<HTMLElement | null>(null)
  const close = useCallback(() => setAnchor(null), [])
  const onOpenChange = props.onOpenChange
  useEffect(() => onOpenChange?.(!!anchor), [anchor, onOpenChange])
  const toggle = (e: React.MouseEvent<HTMLElement>) => {
    if (anchor) return close()
    trigger.current = e.currentTarget
    setSheet(isCoarse())
    setAnchor(e.currentTarget.getBoundingClientRect())
  }
  const triggerOf = useCallback(() => trigger.current, [])
  return (
    <>
      {props.button(!!anchor, toggle)}
      {anchor && <MenuLayer label={props.label} heading={props.heading} anchor={anchor} sheet={sheet} trigger={triggerOf} items={props.items().filter(Boolean) as MenuItem[]} note={props.note} onClose={close} />}
    </>
  )
}

// A menu open (Menu's, or one opened by its owner: a list's rows' menu, at
// a right click): below a button's rect, or from a point (at: the mouse's,
// a zero rect there); a bottom sheet instead where sheet. trigger: the
// button it belongs to, looked up when wanted (focus goes back to it; a
// press on it is not "outside"). heading: a line over its items in the bottom
// sheet, what they are of (a file's name, "switch session"): a popover shows
// beside what opened it, a sheet does not (ux1101 T22).
export function MenuLayer(props: { label: string; heading?: string; anchor: DOMRect; at?: 'below' | 'point'; sheet: boolean; trigger: () => HTMLElement | null; items: MenuItem[]; note?: string; onClose: () => void }) {
  const { onClose } = props
  useEffect(() => {
    window.addEventListener(CLOSE_MENUS, onClose)
    return () => window.removeEventListener(CLOSE_MENUS, onClose)
  }, [onClose])
  return props.sheet ? (
    <Dialog title={props.label} onClose={onClose} hideTitle>
      <div className="handle" aria-hidden="true" />
      {props.heading && <div className="sheet-heading">{props.heading}</div>}
      {props.note && <div className="menu-note">{props.note}</div>}
      <div className="sheet-list" role="menu" onKeyDown={menuKeys}>
        {props.items.map((it, i) => <MenuEntry key={i} item={it} close={onClose} />)}
      </div>
      <button className="btn btn-block" onClick={onClose}>{t('common.cancel')}</button>
    </Dialog>
  ) : (
    <Popover anchor={props.anchor} at={props.at ?? 'below'} trigger={props.trigger} onClose={onClose} label={props.label}>
      {props.note && <div className="menu-note">{props.note}</div>}
      {props.items.map((it, i) => <MenuEntry key={i} item={it} close={onClose} />)}
    </Popover>
  )
}

function MenuEntry({ item, close }: { item: MenuItem; close: () => void }) {
  const cls = 'menu-item' + (item.danger ? ' danger' : '')
  const body = (
    <>
      {item.icon}
      {item.hint ? (
        <span className="menu-item-text">
          <span>{item.label}</span>
          <span className="menu-hint">{item.hint}</span>
        </span>
      ) : (
        <span>{item.label}</span>
      )}
      {item.keys?.length ? (
        <span className="menu-keys" aria-hidden="true">
          {item.keys.map((k) => (
            <kbd key={k}>{k}</kbd>
          ))}
        </span>
      ) : null}
      {item.trail ? (
        <span className="menu-trail" aria-hidden="true">
          {item.trail}
        </span>
      ) : null}
    </>
  )
  return (
    <>
      {item.separator && <hr />}
      {item.group && <div className="menu-group" role="presentation">{item.group}</div>}
      {item.actions?.length ? (
        <div className="menu-item menu-item-actions" role="group" aria-label={item.label}>
          {item.icon}
          <span className="menu-item-actions-label">{item.label}</span>
          <span className="menu-act-group">
            {item.actions.map((a, idx) => (
              <button
                key={idx}
                type="button"
                className={'menu-act-btn' + (a.text ? ' is-text' : '') + (a.on ? ' is-on' : '')}
                role={a.on === undefined ? 'menuitem' : 'menuitemradio'}
                aria-checked={a.on}
                disabled={a.disabled}
                aria-label={a.label}
                title={a.label}
                onClick={() => {
                  if (!item.keepOpen) close()
                  a.onSelect()
                }}
              >
                {a.icon ?? a.text}
              </button>
            ))}
          </span>
        </div>
      ) : item.href ? (
        <a className={cls} role="menuitem" href={item.href} onClick={(e) => { close(); if (item.onSelect) { e.preventDefault(); item.onSelect() } }}>{body}</a>
      ) : (
        <button className={cls} role={item.checked === undefined ? 'menuitem' : 'menuitemcheckbox'} aria-checked={item.checked} disabled={item.disabled} title={item.title} onClick={() => { if (!item.keepOpen) close(); item.onSelect?.() }}>{body}</button>
      )}
    </>
  )
}

// Arrow keys, Home and End move between menu items (the ARIA menu pattern);
// Tab leaves the menu and closes it.
// (Only items that can be chosen: a greyed one is passed over — pages-1105 4.4.)
const PICKABLE = 'button.menu-item:not(:disabled):not([aria-disabled="true"]), a.menu-item:not([aria-disabled="true"]), .menu-act-btn:not(:disabled)'
function menuKeys(e: React.KeyboardEvent<HTMLElement>) {
  const items = [...e.currentTarget.querySelectorAll<HTMLElement>(PICKABLE)]
  if (items.length === 0) return
  const i = items.indexOf(document.activeElement as HTMLElement)
  let next = -1
  if (e.key === 'ArrowDown') next = (i + 1) % items.length
  else if (e.key === 'ArrowUp') next = (i - 1 + items.length) % items.length
  else if (e.key === 'Home') next = 0
  else if (e.key === 'End') next = items.length - 1
  if (next < 0) return
  e.preventDefault()
  items[next].focus()
}

function Popover({ anchor, at, trigger, onClose, label, children }: { anchor: DOMRect; at: 'below' | 'point'; trigger: () => HTMLElement | null; onClose: () => void; label: string; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null)
  const isTop = useLayer()
  const [pos, setPos] = useState<{ top: number; left: number; maxHeight: number } | null>(null)
  // Placed below the trigger if it fits, else above, else wherever there is
  // more room with the list scrolling inside. Re-placed when the content
  // changes size (items that load after opening).
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const place = () => {
      const w = el.offsetWidth
      const h = el.scrollHeight
      const gap = at === 'point' ? 2 : 6
      const below = window.innerHeight - anchor.bottom - 8 - gap
      const above = anchor.top - 8 - gap
      const down = h <= below || below >= above
      const maxHeight = Math.max(120, down ? below : above)
      const top = down ? anchor.bottom + gap : Math.max(8, anchor.top - gap - Math.min(h, maxHeight))
      // (From a point: its top left corner there, as a system's context menu.)
      const left = Math.min(Math.max(8, at === 'point' ? anchor.left : anchor.right - w), window.innerWidth - w - 8)
      setPos((p) => (p && p.top === top && p.left === left && p.maxHeight === maxHeight ? p : { top, left, maxHeight }))
    }
    place()
    const ro = new ResizeObserver(place)
    ro.observe(el)
    return () => ro.disconnect()
  }, [anchor, at])
  useEffect(() => {
    // The trigger toggles the menu itself; treating it as "outside" would
    // close on pointerdown and reopen on click.
    const outside = (t: EventTarget | null) => !ref.current?.contains(t as Node) && !trigger()?.contains(t as Node)
    const onDown = (e: PointerEvent) => {
      if (!outside(e.target)) return
      // (A press of the main button only is followed by a click to swallow: a right press makes none, and the next
      // click — a row's right click opening its own menu, say — is not this press's.)
      if (e.button === 0) swallowNextClick()
      onClose()
    }
    const onKey = (e: KeyboardEvent) => {
      if (!isTop()) return
      if (e.key === 'Escape') {
        e.stopPropagation()
        onClose()
        trigger()?.focus()
      } else if (e.key === 'Tab') {
        onClose()
        trigger()?.focus()
      }
    }
    const onScroll = (e: Event) => outside(e.target) && onClose()
    document.addEventListener('pointerdown', onDown, true)
    document.addEventListener('keydown', onKey, true)
    window.addEventListener('resize', onClose)
    window.addEventListener('scroll', onScroll, true)
    ref.current?.querySelector<HTMLElement>(PICKABLE)?.focus()
    return () => {
      document.removeEventListener('pointerdown', onDown, true)
      document.removeEventListener('keydown', onKey, true)
      window.removeEventListener('resize', onClose)
      window.removeEventListener('scroll', onScroll, true)
    }
  }, [onClose, trigger, isTop])
  return createPortal(
    <div ref={ref} className="menu" role="menu" aria-label={label} onKeyDown={menuKeys} style={pos ? { top: pos.top, left: pos.left, maxHeight: pos.maxHeight } : { top: -9999, left: -9999 }}>
      {children}
    </div>,
    document.body,
  )
}

// Toasts ---------------------------------------------------------------------

// A toast's button: what the user can do about it (it stays a while longer).
export interface ToastAction {
  label: string
  run: () => void
}

// Top-right corner notice variant (desktop-style notification with app icon,
// two-line title + body, optional action button, and close button).
export interface ToastNotice {
  title: string
  body?: string
  icon?: string
  action?: ToastAction
  error?: boolean
  durationMs?: number
}

export type ToastFn = {
  (text: string, error?: boolean, action?: ToastAction): void
  notice: (n: ToastNotice) => void
}

interface ToastItem {
  id: number
  text: string
  title?: string
  icon?: string
  corner?: boolean
  error?: boolean
  action?: ToastAction
}

const noopToast = Object.assign((() => {}) as (text: string, error?: boolean, action?: ToastAction) => void, {
  notice: () => {},
})
const ToastCtx = createContext<ToastFn>(noopToast)

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])
  const drop = useCallback((id: number) => setItems((xs) => xs.filter((x) => x.id !== id)), [])
  const push = useCallback<ToastFn>(
    Object.assign(
      (text: string, error?: boolean, action?: ToastAction) => {
        const id = Date.now() + Math.random()
        setItems((xs) => {
          const corners = xs.filter((x) => x.corner)
          const normal = xs.filter((x) => !x.corner).slice(-2)
          return [...corners, ...normal, { id, text, error, action }]
        })
        setTimeout(() => drop(id), action ? 12000 : error ? 5000 : 2600)
      },
      {
        notice: (n: ToastNotice) => {
          const id = Date.now() + Math.random()
          setItems((xs) => {
            const normal = xs.filter((x) => !x.corner)
            const corners = xs.filter((x) => x.corner).slice(-2)
            return [
              ...normal,
              ...corners,
              {
                id,
                corner: true,
                title: n.title,
                text: n.body ?? '',
                icon: n.icon ?? '/icons/icon.svg',
                error: n.error,
                action: n.action,
              },
            ]
          })
          setTimeout(() => drop(id), n.durationMs ?? (n.action ? 10000 : 6000))
        },
      },
    ),
    [drop],
  )
  const normal = items.filter((i) => !i.corner)
  const corners = items.filter((i) => i.corner)
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toasts" aria-live="polite">
        {normal.map((i) => (
          <div key={i.id} className={'toast' + (i.error ? ' error' : '') + (i.action ? ' has-action' : '')} role={i.error ? 'alert' : 'status'}>
            {i.error ? <CircleAlert size={16} /> : i.action ? <Info size={16} /> : <CircleCheck size={16} />}
            <span>{i.text}</span>
            {i.action && (
              <button
                type="button"
                className="btn toast-action"
                onClick={() => {
                  drop(i.id)
                  i.action!.run()
                }}
              >
                {i.action.label}
              </button>
            )}
          </div>
        ))}
      </div>
      {corners.length > 0 && (
        <div className="toasts-corner" aria-live="polite">
          {corners.map((i) => (
            <div key={i.id} className="toast-note" role={i.error ? 'alert' : 'status'}>
              <img className="toast-note-icon" src={i.icon || '/icons/icon.svg'} alt="" width={28} height={28} aria-hidden="true" />
              <div className="toast-note-text">
                <strong className="toast-note-title">{i.title}</strong>
                {i.text && <span className="toast-note-body">{i.text}</span>}
              </div>
              <div className="toast-note-acts">
                {i.action && (
                  <button
                    type="button"
                    className="toast-note-btn"
                    onClick={() => {
                      drop(i.id)
                      i.action!.run()
                    }}
                  >
                    {i.action.label}
                  </button>
                )}
                <button
                  type="button"
                  className="toast-note-close"
                  onClick={() => drop(i.id)}
                  aria-label={t('common.close')}
                  title={t('common.close')}
                >
                  <X size={14} />
                </button>
              </div>
            </div>
          ))}
        </div>
      )}
    </ToastCtx.Provider>
  )
}

export function useToast() {
  return useContext(ToastCtx)
}

// Copy -------------------------------------------------------------------------

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    // Fallback for contexts without the async clipboard API.
    const ta = document.createElement('textarea')
    ta.value = text
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    const ok = document.execCommand('copy')
    ta.remove()
    return ok
  }
}

export function CopyButton({ text, label }: { text: string; label?: string }) {
  const [done, setDone] = useState(false)
  return (
    <button
      type="button"
      className={label ? 'btn btn-ghost' : 'icon-btn'}
      aria-label={label ?? t('common.copy')}
      title={label ?? t('common.copy')}
      onClick={async (e) => {
        e.stopPropagation()
        if (await copyText(text)) {
          setDone(true)
          setTimeout(() => setDone(false), 1500)
        }
      }}
    >
      {done ? <Check size={16} /> : <Copy size={16} />}
      {label && <span>{done ? t('common.copied') : label}</span>}
    </button>
  )
}

export function Field(props: { label: string; htmlFor?: string; optional?: boolean; hint?: ReactNode; error?: string; children: ReactNode }) {
  // The field's input is told where its hint or error is (read out with it).
  const desc = props.htmlFor && (props.error || props.hint) ? props.htmlFor + '-desc' : undefined
  const c = props.children
  const child = desc && isValidElement<{ id?: string; 'aria-describedby'?: string }>(c) && c.props.id === props.htmlFor && !c.props['aria-describedby'] ? cloneElement(c, { 'aria-describedby': desc }) : c
  return (
    <div className="field">
      <label htmlFor={props.htmlFor}>
        {props.label}
        {props.optional && <span className="opt">{t('common.optional')}</span>}
      </label>
      {child}
      {props.error ? (
        // (Read out as it appears, not only when its field is next reached: ux1101 PG-05.)
        <p className="err" role="alert" id={props.htmlFor ? props.htmlFor + '-desc' : undefined}>
          {props.error}
        </p>
      ) : props.hint ? (
        <p className="hint" id={props.htmlFor ? props.htmlFor + '-desc' : undefined}>
          {props.hint}
        </p>
      ) : null}
    </div>
  )
}

// States -----------------------------------------------------------------------

export function EmptyState(props: { icon: ReactNode; title: string; body?: string; action?: ReactNode }) {
  return (
    <div className="empty">
      {props.icon}
      <h2>{props.title}</h2>
      {props.body && <p>{props.body}</p>}
      {props.action}
    </div>
  )
}

export function LoadError({ onRetry, text }: { onRetry: () => void; text?: string }) {
  return (
    <p className="inline-error" role="alert">
      <span>{text ?? t('common.loadFailed')}</span>
      <button type="button" className="link-btn" onClick={onRetry}>{t('common.retry')}</button>
    </p>
  )
}

// The product mark: the same prompt glyph as the app icon.
export function BrandMark() {
  return (
    <svg className="brand-mark" viewBox="0 0 512 512" aria-hidden="true">
      <rect width="512" height="512" rx="112" fill="var(--accent)" />
      <path d="M150 176l96 80-96 80" fill="none" stroke="#fff" strokeWidth="44" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M276 336h96" stroke="#fff" strokeWidth="44" strokeLinecap="round" />
    </svg>
  )
}
