import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { api, ApiError, errorText } from '../api'
import { t, type I18nKey } from '../i18n'
import { cardLooker } from './cardLooker'

// Claude Code's card above its input box (its you-should-know plugin: "Heads
// up", "You should know"), read off the pane's screen by the gateway (see
// internal/web/convo_card.go and plans/headsup-card.md) and shown over the
// composer, folded to a line; its keys pressed from here where the gateway
// finds the card and an empty box as shown.

export interface ClaudeCard {
  state: 'offer' | 'explained' | 'making' | 'folded' | 'ready' | 'failed' | 'other'
  tag?: string
  text: string
  choices: { key: string; label: string }[] | null
  actionable: boolean
  empty: boolean
}

type Body = { pane: string; pid: number; start: number; session: string }

// useClaudeCard: the card of the conversation in front (key: which one; ''
// none), looked for on its own times (cardLooker.ts); shared: a look of the
// suggestion's begun, the card in its answer to be taken in.
export function useClaudeCard(sid: string, key: string, turnKey: string, body: () => Body | null) {
  const [card, setCard] = useState<ClaudeCard | null>(null)
  const [stale, setStale] = useState(false) // (the last look failed: shown as it was, not to be answered)
  const now = useRef({ sid, body })
  now.current = { sid, body }
  const looker = useMemo(
    () =>
      cardLooker<ClaudeCard>({
        fetch: () =>
          api<{ card?: ClaudeCard }>('POST', `/api/sessions/${now.current.sid}/conversation/suggestion`, now.current.body(), { timeoutMs: 20000 }).then(
            (r) => r.card ?? null,
          ),
        // (Not now: the page not in front, or nothing to look at.)
        away: () => document.hidden || !now.current.body(),
        setTimeout: (f, ms) => setTimeout(f, ms),
        clearTimeout: (t) => clearTimeout(t as ReturnType<typeof setTimeout>),
        setInterval: (f, ms) => setInterval(f, ms),
        clearInterval: (t) => clearInterval(t as ReturnType<typeof setInterval>),
        now: () => Date.now(),
        found: (c) => {
          setCard(c)
          setStale(false)
        },
        failed: () => setStale(true),
      }),
    [],
  )
  useEffect(() => () => looker.stop(), [looker])
  // Another conversation: nothing of the last one's.
  useEffect(() => {
    setCard(null)
    setStale(false)
    looker.bind(key)
    const onSeen = () => looker.shown()
    document.addEventListener('visibilitychange', onSeen)
    return () => document.removeEventListener('visibilitychange', onSeen)
  }, [key, looker])
  // After a turn's end.
  useEffect(() => {
    if (key && turnKey) looker.turnEnded()
  }, [key, turnKey, looker])
  // (A look of the suggestion's begun, the card in its answer: see cardLooker's shared.)
  const shared = useCallback(() => looker.shared(), [looker])
  const press = useCallback(
    async (k: string): Promise<void> => {
      const b = now.current.body()
      if (!b || !card) return
      await api('POST', `/api/sessions/${sid}/conversation/card`, { ...b, key: k, card: { state: card.state, tag: card.tag ?? '', text: card.text, choices: card.choices ?? [] } }, { timeoutMs: 20000 })
      looker.pressed()
    },
    [sid, card, looker],
  )
  return { card, stale, shared, press }
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

const REFUSED: Record<string, I18nKey> = {
  card_gone: 'card.gone',
  card_changed: 'card.changed',
  screen_changed: 'card.changed',
  card_box_typed: 'card.typed',
  card_unreadable: 'card.unreadable',
  card_just_pressed: 'card.justPressed',
  agent_waiting: 'card.waiting',
}

export function CardNotice({ card, stale, onPress }: { card: ClaudeCard; stale: boolean; onPress: (key: string) => Promise<void> }) {
  // (Folded to a line at first; opened again for the same words. Opened by
  // itself once "Learn more" was pressed: its explanation is what was asked.)
  const [open, setOpen] = useState(false)
  const openFor = useRef<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [said, setSaid] = useState('')
  useEffect(() => {
    if (openFor.current !== null && openFor.current !== card.text) {
      setOpen(true)
      openFor.current = null
    }
    setSaid('')
  }, [card.text])
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
              {card.choices.map((c) => (
                <button
                  key={c.key}
                  type="button"
                  className="btn btn-ghost claude-card-key"
                  disabled={!!why || busy}
                  onClick={() => {
                    setBusy(true)
                    setSaid('')
                    if (c.label === 'Learn more') openFor.current = card.text
                    onPress(c.key)
                      .catch((e) => setSaid(e instanceof ApiError && REFUSED[e.code] ? t(REFUSED[e.code]) : errorText(e)))
                      .finally(() => setBusy(false))
                  }}
                >
                  {LABELS[c.label] ? t(LABELS[c.label]) : c.label}
                </button>
              ))}
            </div>
          )}
          {(why || said) && <p className="claude-card-why">{said || why}</p>}
        </div>
      )}
    </div>
  )
}
