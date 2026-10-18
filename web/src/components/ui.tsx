import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { Check, CircleAlert, CircleCheck, Copy } from 'lucide-react'
import { t } from '../i18n'

export const isCoarse = () => window.matchMedia('(pointer: coarse)').matches

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
    const focusables = () =>
      [...(root?.querySelectorAll<HTMLElement>('a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])') ?? [])]
        .filter((el) => el.offsetParent !== null || el === document.activeElement)
    const first = root?.querySelector<HTMLElement>('input, textarea, select') ?? focusables()[0]
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
      prev?.focus?.()
    }
  }, [])
  return ref
}

// Dialog ---------------------------------------------------------------------

export function Dialog(props: { title: string; onClose: () => void; children: ReactNode; sheet?: boolean; hideTitle?: boolean }) {
  const ref = useFocusTrap<HTMLDivElement>(props.onClose)
  return createPortal(
    <div className={'overlay' + (props.sheet !== false ? ' sheet-mode' : '')} onMouseDown={(e) => e.target === e.currentTarget && props.onClose()}>
      <div className="dialog" role="dialog" aria-modal="true" aria-label={props.title} ref={ref}>
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
  return (
    <Dialog title={props.title} onClose={props.onClose}>
      <p className="lead">{props.body}</p>
      {error && <p className="err">{error}</p>}
      <div className="dialog-actions">
        <button className="btn" onClick={props.onClose}>{t('common.cancel')}</button>
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
}

export function Menu(props: { label: string; button: (open: boolean, toggle: (e: React.MouseEvent<HTMLElement>) => void) => ReactNode; items: () => (MenuItem | null)[]; note?: string }) {
  const [anchor, setAnchor] = useState<DOMRect | null>(null)
  const [sheet, setSheet] = useState(false)
  const trigger = useRef<HTMLElement | null>(null)
  const close = useCallback(() => setAnchor(null), [])
  const toggle = (e: React.MouseEvent<HTMLElement>) => {
    if (anchor) return close()
    trigger.current = e.currentTarget
    setSheet(isCoarse())
    setAnchor(e.currentTarget.getBoundingClientRect())
  }
  const items = anchor ? (props.items().filter(Boolean) as MenuItem[]) : []
  return (
    <>
      {props.button(!!anchor, toggle)}
      {anchor &&
        (sheet ? (
          <Dialog title={props.label} onClose={close} hideTitle>
            <div className="handle" aria-hidden="true" />
            {props.note && <div className="menu-note">{props.note}</div>}
            <div className="sheet-list" role="menu">
              {items.map((it, i) => <MenuEntry key={i} item={it} close={close} />)}
            </div>
            <button className="btn btn-block" onClick={close}>{t('common.cancel')}</button>
          </Dialog>
        ) : (
          <Popover anchor={anchor} trigger={trigger.current} onClose={close} label={props.label}>
            {props.note && <div className="menu-note">{props.note}</div>}
            {items.map((it, i) => <MenuEntry key={i} item={it} close={close} />)}
          </Popover>
        ))}
    </>
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
        <button className={cls} role="menuitem" onClick={() => { if (!item.keepOpen) close(); item.onSelect?.() }}>{body}</button>
      )}
    </>
  )
}

function Popover({ anchor, trigger, onClose, label, children }: { anchor: DOMRect; trigger: HTMLElement | null; onClose: () => void; label: string; children: ReactNode }) {
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
      const below = window.innerHeight - anchor.bottom - 14
      const above = anchor.top - 14
      const down = h <= below || below >= above
      const maxHeight = Math.max(120, down ? below : above)
      const top = down ? anchor.bottom + 6 : Math.max(8, anchor.top - 6 - Math.min(h, maxHeight))
      const left = Math.min(Math.max(8, anchor.right - w), window.innerWidth - w - 8)
      setPos((p) => (p && p.top === top && p.left === left && p.maxHeight === maxHeight ? p : { top, left, maxHeight }))
    }
    place()
    const ro = new ResizeObserver(place)
    ro.observe(el)
    return () => ro.disconnect()
  }, [anchor])
  useEffect(() => {
    // The trigger toggles the menu itself; treating it as "outside" would
    // close on pointerdown and reopen on click.
    const outside = (t: EventTarget | null) => !ref.current?.contains(t as Node) && !trigger?.contains(t as Node)
    const onDown = (e: PointerEvent) => outside(e.target) && onClose()
    const onKey = (e: KeyboardEvent) => {
      if (!isTop()) return
      if (e.key === 'Escape') {
        e.stopPropagation()
        onClose()
        trigger?.focus()
      } else if (e.key === 'Tab') {
        onClose()
        trigger?.focus()
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
    <div ref={ref} className="menu" role="menu" aria-label={label} style={pos ? { top: pos.top, left: pos.left, maxHeight: pos.maxHeight } : { top: -9999, left: -9999 }}>
      {children}
    </div>,
    document.body,
  )
}

// Toasts ---------------------------------------------------------------------

interface ToastItem {
  id: number
  text: string
  error?: boolean
}

const ToastCtx = createContext<(text: string, error?: boolean) => void>(() => {})

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])
  const push = useCallback((text: string, error?: boolean) => {
    const id = Date.now() + Math.random()
    setItems((xs) => [...xs.slice(-2), { id, text, error }])
    setTimeout(() => setItems((xs) => xs.filter((x) => x.id !== id)), error ? 5000 : 2600)
  }, [])
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toasts" aria-live="polite">
        {items.map((i) => (
          <div key={i.id} className={'toast' + (i.error ? ' error' : '')} role={i.error ? 'alert' : 'status'}>
            {i.error ? <CircleAlert size={16} /> : <CircleCheck size={16} />}
            <span>{i.text}</span>
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
  return (
    <div className="field">
      <label htmlFor={props.htmlFor}>
        {props.label}
        {props.optional && <span className="opt">{t('common.optional')}</span>}
      </label>
      {props.children}
      {props.error ? <p className="err">{props.error}</p> : props.hint ? <p className="hint">{props.hint}</p> : null}
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
      <button className="link-btn" onClick={onRetry}>{t('common.retry')}</button>
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
