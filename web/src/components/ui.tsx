import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react'
import { Check, Copy } from 'lucide-react'
import { t } from '../i18n'

// Focus handling for modal layers: focus moves in on open, Tab stays inside,
// Escape closes, and focus returns to where it was afterwards.
export function useFocusTrap<T extends HTMLElement>(onClose: () => void) {
  const ref = useRef<T>(null)
  const closeRef = useRef(onClose)
  closeRef.current = onClose
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    const root = ref.current
    const focusables = () =>
      [...(root?.querySelectorAll<HTMLElement>('a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])') ?? [])]
        .filter((el) => el.offsetParent !== null || el === document.activeElement)
    const first = root?.querySelector<HTMLElement>('input, textarea, select') ?? focusables()[0]
    first?.focus()
    const onKey = (e: KeyboardEvent) => {
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
      if (e.shiftKey && (i <= 0)) {
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

export function Dialog(props: { title: string; onClose: () => void; children: ReactNode; sheet?: boolean }) {
  const ref = useFocusTrap<HTMLDivElement>(props.onClose)
  return (
    <div className={'overlay' + (props.sheet !== false ? ' sheet-mode' : '')} onMouseDown={(e) => e.target === e.currentTarget && props.onClose()}>
      <div className="dialog" role="dialog" aria-modal="true" aria-label={props.title} ref={ref}>
        <h2>{props.title}</h2>
        {props.children}
      </div>
    </div>
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

// Menu -----------------------------------------------------------------------

export function Menu(props: { button: (open: boolean, toggle: () => void) => ReactNode; children: (close: () => void) => ReactNode; up?: boolean }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const onDown = (e: PointerEvent) => {
      if (!ref.current?.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && setOpen(false)
    document.addEventListener('pointerdown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('pointerdown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])
  const close = useCallback(() => setOpen(false), [])
  return (
    <div className="menu-wrap" ref={ref}>
      {props.button(open, () => setOpen((o) => !o))}
      {open && (
        <div className={'menu' + (props.up ? ' up' : '')} role="menu">
          {props.children(close)}
        </div>
      )}
    </div>
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
          <div key={i.id} className={'toast' + (i.error ? ' error' : '')}>
            {i.text}
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  )
}

export function useToast() {
  return useContext(ToastCtx)
}

// Copy button ----------------------------------------------------------------

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
      className={label ? 'btn' : 'icon-btn'}
      aria-label={label ?? t('common.copy')}
      title={label ?? t('common.copy')}
      onClick={async () => {
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

export function LoadError({ onRetry }: { onRetry: () => void }) {
  return (
    <div className="empty" role="alert">
      <p>{t('common.loadFailed')}</p>
      <button className="btn" onClick={onRetry}>{t('common.retry')}</button>
    </div>
  )
}
