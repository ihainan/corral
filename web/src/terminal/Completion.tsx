import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
import { t } from '../i18n'
import { apply, suggest, tokenAt, type Agent, type Completions, type Suggestion, type Token } from './complete'

const FRESH_MS = 30_000 // a listing is asked for again after this
const NEW_AT_MS = 3_000 // or after this, at a new "@" (the terminal may be in another folder now)

// useCompletion offers, in the composer, what follows "@", "/" or "$" at the
// caret (see complete.ts). The host's listing is asked for when first needed
// and again once it is old.
export function useCompletion(p: {
  sessionId: string
  field: React.RefObject<HTMLTextAreaElement | null>
  draft: string
  setDraft: (s: string) => void
  agent: string | undefined // herdr's focused pane's agent, when known
  pane?: string // (a herdr pane not in front, as the agents page's agent: its folder)
}) {
  const [data, setData] = useState<Completions | null>(null)
  const loadedAt = useRef(0)
  const loading = useRef(false)
  const [failed, setFailed] = useState(false)
  const lastAt = useRef<number | null>(null) // where the last "@" being completed started
  const [tok, setTok] = useState<Token | null>(null)
  const [sel, setSel] = useState(0)
  const [dismissed, setDismissed] = useState<number | null>(null) // the token start Escape closed it at

  const load = useCallback((fresh: number) => {
    if (loading.current || Date.now() - loadedAt.current < fresh) return
    loading.current = true
    api<Completions>('GET', `/api/sessions/${p.sessionId}/complete` + (p.pane ? '?pane=' + encodeURIComponent(p.pane) : ''), undefined, { timeoutMs: 20000 })
      .then((d) => {
        loadedAt.current = Date.now()
        setData(d)
        setFailed(false)
      })
      .catch(() => {
        loadedAt.current = Date.now() - FRESH_MS + 5000 // (tried again in a while)
        setFailed(true)
      })
      .finally(() => (loading.current = false))
  }, [p.sessionId, p.pane])

  // The token at the caret, after each change of the text or the caret.
  const update = useCallback(() => {
    const el = p.field.current
    if (!el || el.selectionStart !== el.selectionEnd || document.activeElement !== el) return setTok(null)
    const next = tokenAt(el.value, el.selectionStart)
    setTok((cur) => (cur && next && cur.start === next.start && cur.query === next.query && cur.kind === next.kind ? cur : next))
    const newAt = next?.kind === '@' && next.start !== lastAt.current
    lastAt.current = next?.kind === '@' ? next.start : null
    if (next) load(newAt ? NEW_AT_MS : FRESH_MS)
  }, [p.field, load])
  useEffect(update, [p.draft, update])
  useEffect(() => setSel(0), [tok?.start, tok?.query])
  // Closed with Escape: until that word is gone (typing on in it keeps it closed).
  useEffect(() => {
    if (!tok || tok.start !== dismissed) setDismissed(null)
  }, [tok, dismissed])

  const a: Agent | null = p.agent === 'claude' || p.agent === 'codex' ? p.agent : null
  const items: Suggestion[] = tok && dismissed !== tok.start ? suggest(tok, data, a) : []
  const cur = Math.min(sel, Math.max(0, items.length - 1)) // (the list may have shrunk under it)
  const waitingFiles = !!tok && tok.kind === '@' && dismissed !== tok.start && !data
  // Asked again and not answered: what is offered is from before (another folder, maybe).
  const staleFiles = !!tok && tok.kind === '@' && dismissed !== tok.start && failed && !!data

  const pick = (i: number) => {
    const el = p.field.current
    const s = items[i]
    if (!el || !tok || !s) return
    const r = apply(el.value, tok, s)
    p.setDraft(r.text)
    requestAnimationFrame(() => {
      el.focus()
      el.selectionStart = el.selectionEnd = r.caret
      update()
    })
  }

  // Keys while the list shows: Up/Down choose, Enter or Tab take, Escape
  // closes the list (not the composer). True when the key was used.
  const onKey = (e: React.KeyboardEvent): boolean => {
    // (With Shift the keys keep their meaning: selecting text, focus back.)
    if (items.length === 0 || e.altKey || e.metaKey || e.ctrlKey || e.shiftKey) return false
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      setSel((cur + (e.key === 'ArrowDown' ? 1 : items.length - 1)) % items.length)
    } else if ((e.key === 'Enter' && !e.shiftKey) || e.key === 'Tab') {
      pick(cur)
    } else if (e.key === 'Escape') {
      setDismissed(tok!.start)
    } else return false
    e.preventDefault()
    return true
  }

  // The field names the list and the option chosen, for screen readers.
  const listId = 'complete-' + p.sessionId
  // (The one chosen with the keys stays in view: a short window shows only some.)
  useEffect(() => {
    if (items.length > 0) document.getElementById(`${listId}-${cur}`)?.scrollIntoView({ block: 'nearest' })
  }, [cur, items.length, listId])
  const field = {
    'aria-autocomplete': 'list' as const,
    'aria-controls': items.length > 0 ? listId : undefined,
    'aria-expanded': items.length > 0,
    'aria-activedescendant': items.length > 0 ? `${listId}-${cur}` : undefined,
  }
  const list =
    items.length > 0 || waitingFiles || staleFiles ? (
      <div className="composer-complete" role="listbox" id={listId} aria-label={t('complete.label')}>
        {waitingFiles && <div className="complete-note">{failed ? t('complete.failed') : t('common.loading')}</div>}
        {staleFiles && <div className="complete-note">{t('complete.failed')}</div>}
        {items.map((s, i) => (
          <button
            key={s.insert}
            id={`${listId}-${i}`}
            type="button"
            role="option"
            aria-selected={i === cur}
            className="complete-item"
            tabIndex={-1}
            onMouseDown={(e) => e.preventDefault()} // (the box keeps the focus and the keyboard)
            onClick={() => pick(i)}
          >
            <span className="complete-label">{s.label}</span>
            {s.hint && <span className="complete-hint">{s.hint}</span>}
          </button>
        ))}
        {tok?.kind === '@' && data?.more && <div className="complete-note">{t('complete.more')}</div>}
      </div>
    ) : null

  return { list, onKey, update, field, open: items.length > 0 }
}
