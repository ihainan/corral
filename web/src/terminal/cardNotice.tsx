import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useEffectOnChange } from '../shown'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { api, ApiError, errorText } from '../api'
import { t, type I18nKey } from '../i18n'
import { cardLooker } from './cardLooker'
import {
  activeBusyKey,
  expireCardSettle,
  failCardPress,
  finishCardPress,
  initialCardState,
  pollCardFailed,
  pollCardFound,
  resetCardState,
  SETTLE_MS,
  startCardPress,
  visibleCard,
  type CardViewState,
  type ClaudeCard,
} from './cardState'

export type { ClaudeCard }

// Claude Code's card above its input box (its you-should-know plugin: "Heads
// up", "You should know"), read off the pane's screen by the gateway (see
// internal/web/convo_card.go and plans/headsup-card.md) and shown over the
// composer, folded to a line; its keys pressed from here where the gateway
// finds the card and an empty box as shown.

type Body = { pane: string; pid: number; start: number; session: string }

const REFUSED: Record<string, I18nKey> = {
  card_gone: 'card.gone',
  card_changed: 'card.changed',
  screen_changed: 'card.changed',
  card_box_typed: 'card.typed',
  card_unreadable: 'card.unreadable',
  card_just_pressed: 'card.justPressed',
  agent_waiting: 'card.waiting',
}

// useClaudeCard: the card of the conversation in front (key: which one; ''
// none), looked for on its own times (cardLooker.ts); shared: a look of the
// suggestion's begun, the card in its answer to be taken in.
export function useClaudeCard(sid: string, key: string, turnKey: string, body: () => Body | null) {
  const [view, setView] = useState<CardViewState>(() => initialCardState())
  const viewRef = useRef(view)
  viewRef.current = view
  const commit = useCallback((next: CardViewState) => {
    viewRef.current = next
    setView(next)
  }, [])
  const update = useCallback((fn: (s: CardViewState) => CardViewState) => {
    const next = fn(viewRef.current)
    if (next !== viewRef.current) {
      viewRef.current = next
      setView(next)
    }
    return next
  }, [])
  const now = useRef({ sid, body })
  now.current = { sid, body }
  const looker = useMemo(
    () =>
      cardLooker<ClaudeCard>({
        fetch: () =>
          // (for=card: said apart from the suggestion's own looks, in the gateway's log and the tests; not read.)
          api<{ card?: ClaudeCard }>('POST', `/api/sessions/${now.current.sid}/conversation/suggestion?for=card`, now.current.body(), { timeoutMs: 20000 }).then(
            (r) => r.card ?? null,
          ),
        // (Not now: the page not in front, or nothing to look at.)
        away: () => document.hidden || !now.current.body(),
        setTimeout: (f, ms) => setTimeout(f, ms),
        clearTimeout: (t) => clearTimeout(t as ReturnType<typeof setTimeout>),
        setInterval: (f, ms) => setInterval(f, ms),
        clearInterval: (t) => clearInterval(t as ReturnType<typeof setInterval>),
        now: () => Date.now(),
        found: (c) => update((s) => pollCardFound(s, c, Date.now())),
        failed: () => update((s) => pollCardFailed(s)),
      }),
    [update],
  )
  useEffect(() => () => looker.stop(), [looker])
  // Another conversation: nothing of the last one's.
  // (Another conversation's: none of the last one's — not as it is shown again from a tab behind, the card kept.)
  useEffectOnChange(() => {
    commit(resetCardState(viewRef.current))
  }, [key])
  useEffect(() => {
    looker.bind(key)
    const onSeen = () => looker.shown()
    document.addEventListener('visibilitychange', onSeen)
    return () => document.removeEventListener('visibilitychange', onSeen)
  }, [key, looker])
  // After a turn's end.
  useEffect(() => {
    if (key && turnKey) looker.turnEnded()
  }, [key, turnKey, looker])
  // End the settle window if the card never changed or disappeared on its own.
  const settleUntil = view.pending?.phase === 'settling' ? view.pending.until : 0
  useEffect(() => {
    if (!settleUntil) return
    const wait = Math.max(0, settleUntil - Date.now())
    const timer = window.setTimeout(() => update((s) => expireCardSettle(s, Date.now())), wait)
    return () => clearTimeout(timer)
  }, [settleUntil, update])
  // (A look of the suggestion's begun, the card in its answer: see cardLooker's shared.)
  const shared = useCallback(() => looker.shared(), [looker])
  const press = useCallback(
    async (k: string, label = ''): Promise<void> => {
      const b = now.current.body()
      if (!b) return
      const cur = viewRef.current
      const started = startCardPress(cur, k, label)
      if (!started.accepted || !cur.card) return
      const target = cur.card
      commit(started.state)
      try {
        await api(
          'POST',
          `/api/sessions/${now.current.sid}/conversation/card`,
          { ...b, key: k, card: { state: target.state, tag: target.tag ?? '', text: target.text, choices: target.choices ?? [] } },
          { timeoutMs: 20000 },
        )
        const after = update((s) => finishCardPress(s, started.pressId, Date.now(), SETTLE_MS))
        if (after.pending?.id === started.pressId) looker.pressed()
      } catch (e) {
        const code = e instanceof ApiError ? e.code : ''
        const msg = e instanceof ApiError && REFUSED[e.code] ? t(REFUSED[e.code]) : errorText(e)
        update((s) => failCardPress(s, started.pressId, code, msg))
        if (code === 'card_changed' || code === 'screen_changed') looker.look()
      }
    },
    [commit, update, looker],
  )
  const ts = Date.now()
  return {
    card: visibleCard(view, ts),
    stale: view.stale,
    busyKey: activeBusyKey(view, ts),
    error: view.error,
    errorSeq: view.errorSeq,
    shared,
    press,
  }
}

const LABELS: Record<string, I18nKey> = {
  'Learn more': 'card.learn',
  'Knew this already': 'card.knew',
  'Know this already': 'card.knew',
  Dismiss: 'card.dismiss',
  Understood: 'card.understood',
  'Chat in main session': 'card.chat',
  Disable: 'card.disable',
  Helpful: 'card.helpful',
  'Not helpful': 'card.notHelpful',
  'Didn’t read': 'card.unread',
  "Didn't read": 'card.unread',
  OK: 'card.ok',
}
const TAGS: Record<string, I18nKey> = { 'Heads up': 'card.headsUp', 'You should know': 'card.shouldKnow' }

export function CardNotice({
  card,
  stale,
  busyKey,
  error,
  errorSeq,
  onPress,
}: {
  card: ClaudeCard
  stale: boolean
  busyKey?: string | null
  error?: string
  errorSeq?: number
  onPress: (key: string, label: string) => Promise<void>
}) {
  // (Folded to a line at first; opened again for the same words. Opened by
  // itself once "Learn more" was pressed: its explanation is what was asked,
  // or when a dismissed card is restored because the background press failed.)
  const [open, setOpen] = useState(() => !!error)
  const openFor = useRef<string | null>(null)
  useEffect(() => {
    if (openFor.current !== null && openFor.current !== card.text) {
      setOpen(true)
      openFor.current = null
    }
  }, [card.text])
  useEffect(() => {
    if (error) setOpen(true)
  }, [error, errorSeq])
  const busy = !!busyKey
  const tag = card.tag ? (TAGS[card.tag] ? t(TAGS[card.tag]) : card.tag) : ''
  const first = card.text.split('\n')[0]
  const why = stale ? t('card.stale') : !card.actionable ? t('card.unreadable') : !card.empty ? t('card.typed') : ''
  const state = card.state === 'making' ? t('card.making') : card.state === 'folded' ? t('card.folded') : ''
  return (
    <div className={'claude-card' + (open ? ' is-open' : '')} role="note">
      <button type="button" className="claude-card-head" aria-expanded={open} onClick={() => setOpen(!open)}>
        <span className="claude-card-star" aria-hidden="true">
          ✦
        </span>
        {tag && <span className="claude-card-tag">{tag}</span>}
        {/* (Opened, its words are under it whole: not a cut line of them here too.) */}
        <span className="claude-card-first">{state || (open ? '' : first)}</span>
        {open ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
      </button>
      {open && (
        <div className="claude-card-body">
          {!state && <p className="claude-card-text">{card.text}</p>}
          {!!card.choices?.length && (
            <div className="claude-card-keys">
              {card.choices.map((c) => {
                const pressing = busyKey === c.key
                return (
                  <button
                    key={c.key}
                    type="button"
                    className={'btn btn-ghost claude-card-key' + (pressing ? ' is-busy' : '')}
                    aria-busy={pressing || undefined}
                    disabled={!!why || busy}
                    onClick={() => {
                      if (c.label === 'Learn more') openFor.current = card.text
                      void onPress(c.key, c.label)
                    }}
                  >
                    {pressing ? t('card.sending') : LABELS[c.label] ? t(LABELS[c.label]) : c.label}
                  </button>
                )
              })}
            </div>
          )}
          {(why || error) && <p className="claude-card-why">{error || why}</p>}
        </div>
      )}
    </div>
  )
}
