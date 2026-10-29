import { cloneElement, createContext, isValidElement, useCallback, useContext, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { Check, CircleAlert, CircleCheck, Copy, Info } from 'lucide-react'
import { t } from '../i18n'

export const isCoarse = () => window.matchMedia('(pointer: coarse)').matches

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
    const first = root?.querySelector<HTMLElement>(`input${skip}, textarea${skip}, select${skip}`) ?? focusables().find((el) => !(isCoarse() && el.hasAttribute('data-touch-nofocus')))
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
export function Dialog(props: { title: string; onClose: () => void; children: ReactNode; sheet?: boolean; hideTitle?: boolean; wide?: boolean; seeThrough?: boolean; className?: string; dialogProps?: React.HTMLAttributes<HTMLDivElement> }) {
  const ref = useFocusTrap<HTMLDivElement>(props.onClose)
  return createPortal(
    <div className={'overlay' + (props.sheet !== false ? ' sheet-mode' : '') + (props.seeThrough ? ' see-through' : '')} onMouseDown={(e) => e.target === e.currentTarget && props.onClose()}>
      <div {...props.dialogProps} className={'dialog' + (props.wide ? ' dialog-wide' : '') + (props.className ? ' ' + props.className : '')} role="dialog" aria-modal="true" aria-label={props.title} ref={ref}>
        {props.hideTitle ? null : <h2>{props.title}</h2>}
        {props.children}
      </div>
    </div>,
    document.body,
  )
}

export function Confirm(props: { title: string; body: string; confirm: string; danger?: boolean; onConfirm: () => Promise<void> | void; onClose: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // Once confirmed, the request cannot be called back: the dialog stays
  // until it is answered (closing it would look like cancelling).
  const close = () => !busy && props.onClose()
  return (
    <Dialog title={props.title} onClose={close}>
      <p className="lead">{props.body}</p>
      {error && <p className="err">{error}</p>}
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
              setError(e?.human ?? String(e))
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
}

// Sent on the window to close every open menu (a terminal tab sent to the back).
export const CLOSE_MENUS = 'corral:close-menus'

export function Menu(props: { label: string; button: (open: boolean, toggle: (e: React.MouseEvent<HTMLElement>) => void) => ReactNode; items: () => (MenuItem | null)[]; note?: string; onOpenChange?: (open: boolean) => void }) {
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
      {anchor && <MenuLayer label={props.label} anchor={anchor} sheet={sheet} trigger={triggerOf} items={props.items().filter(Boolean) as MenuItem[]} note={props.note} onClose={close} />}
    </>
  )
}

// A menu open (Menu's, or one opened by its owner: a list's rows' menu, at
// a right click): below a button's rect, or from a point (at: the mouse's,
// a zero rect there); a bottom sheet instead where sheet. trigger: the
// button it belongs to, looked up when wanted (focus goes back to it; a
// press on it is not "outside").
export function MenuLayer(props: { label: string; anchor: DOMRect; at?: 'below' | 'point'; sheet: boolean; trigger: () => HTMLElement | null; items: MenuItem[]; note?: string; onClose: () => void }) {
  const { onClose } = props
  useEffect(() => {
    window.addEventListener(CLOSE_MENUS, onClose)
    return () => window.removeEventListener(CLOSE_MENUS, onClose)
  }, [onClose])
  return props.sheet ? (
    <Dialog title={props.label} onClose={onClose} hideTitle>
      <div className="handle" aria-hidden="true" />
      {props.note && <div className="menu-note">{props.note}</div>}
      <div className="sheet-list" role="menu">
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
      <span>{item.label}</span>
    </>
  )
  return (
    <>
      {item.separator && <hr />}
      {item.href ? (
        <a className={cls} role="menuitem" href={item.href} onClick={(e) => { close(); if (item.onSelect) { e.preventDefault(); item.onSelect() } }}>{body}</a>
      ) : (
        <button className={cls} role="menuitem" disabled={item.disabled} title={item.title} onClick={() => { if (!item.keepOpen) close(); item.onSelect?.() }}>{body}</button>
      )}
    </>
  )
}

// Arrow keys, Home and End move between menu items (the ARIA menu pattern);
// Tab leaves the menu and closes it.
function menuKeys(e: React.KeyboardEvent<HTMLElement>) {
  const items = [...e.currentTarget.querySelectorAll<HTMLElement>('.menu-item')]
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
    const onDown = (e: PointerEvent) => outside(e.target) && onClose()
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
    ref.current?.querySelector<HTMLElement>('.menu-item')?.focus()
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

interface ToastItem {
  id: number
  text: string
  error?: boolean
  action?: ToastAction
}

const ToastCtx = createContext<(text: string, error?: boolean, action?: ToastAction) => void>(() => {})

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])
  const drop = useCallback((id: number) => setItems((xs) => xs.filter((x) => x.id !== id)), [])
  const push = useCallback((text: string, error?: boolean, action?: ToastAction) => {
    const id = Date.now() + Math.random()
    setItems((xs) => [...xs.slice(-2), { id, text, error, action }])
    setTimeout(() => drop(id), action ? 12000 : error ? 5000 : 2600)
  }, [drop])
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toasts" aria-live="polite">
        {items.map((i) => (
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
        <p className="err" id={props.htmlFor ? props.htmlFor + '-desc' : undefined}>
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
