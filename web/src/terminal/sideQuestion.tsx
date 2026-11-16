import { useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { MessageCircleQuestion, RotateCw } from 'lucide-react'
import { api, ApiError } from '../api'
import { t, type I18nKey } from '../i18n'
import { CopyButton, Dialog, DialogHead } from '../components/ui'
import { isDark, usePrefs } from '../prefs'
import { Markdown } from './markdown'
import { onMac } from './pageKeys'

// A side question (Claude Code's /btw, from here): a question of the
// conversation as it stands, answered beside it by a copy of the session
// that is not kept (the gateway's conversation/side) — the agent not
// interrupted, nothing written into its log. The last few of each agent
// are kept while the page is open, in memory only: closed and opened
// again, they are still there; one still being answered too.

export type SideBind = { pane: string; pid: number; start: number; session: string }

type Asked = {
  id: number
  q: string
  at: number // (when asked, by this page's clock)
  state: 'asking' | 'ok' | 'failed'
  answer?: string
  why?: I18nKey
  said?: string // (Claude Code's own word, where it gave one in place of an answer)
  ms?: number
  cost?: number
}

const KEEP = 5
const AGENTS = 8 // (of so many agents at most: those asked of last)
export const SIDE_MAX = 2000 // (runes, as the gateway counts them)
// (The gateway's run is 90s on the host, and 100s in all: a little past that.)
const ASK_MS = 110_000

// Kept per agent: the Corral session, the pane, the process and its session.
const kept = new Map<string, Asked[]>()
const heard = new Set<() => void>()
let ids = 0
const tell = () => heard.forEach((f) => f())
const keyOf = (sid: string, b: SideBind) => [sid, b.pane, b.pid, b.start, b.session].join(' ')

function put(key: string, a: Asked) {
  const list = (kept.get(key) ?? []).filter((x) => x.id !== a.id)
  kept.delete(key) // (to the end: the one asked of last)
  kept.set(key, [...list, a].slice(-KEEP))
  // (The oldest dropped first; never this one, nor one still being answered.)
  for (const k of [...kept.keys()]) {
    if (kept.size <= AGENTS) break
    if (k !== key && !kept.get(k)!.some((x) => x.state === 'asking')) kept.delete(k)
  }
  tell()
}

const whyOf: Record<string, I18nKey> = {
  timeout: 'side.err.timeout',
  busy: 'side.err.busy',
  agent_changed: 'side.err.gone',
  gone: 'side.err.gone',
  noclaude: 'side.err.noclaude',
}

// sideModel: what the gateway takes as a model's name (the log's own, of
// it: "<synthetic>" and the like are not sent — the copy then takes its default).
const sideModel = /^[A-Za-z0-9][A-Za-z0-9._:/@[\]-]{0,127}$/

function ask(sid: string, b: SideBind, model: string | undefined, q: string, again?: number) {
  const key = keyOf(sid, b)
  const a: Asked = { id: again ?? ++ids, q, at: Date.now(), state: 'asking' }
  put(key, a)
  const body = { ...b, question: q, ...(model && sideModel.test(model) ? { model } : {}) }
  api<{ ok: boolean; answer?: string; reason?: string; duration_ms?: number; cost_usd?: number }>('POST', `/api/sessions/${sid}/conversation/side`, body, { timeoutMs: ASK_MS }).then(
    (r) => {
      if (r.ok) put(key, { ...a, state: 'ok', answer: r.answer ?? '', ms: r.duration_ms, cost: r.cost_usd })
      else put(key, { ...a, state: 'failed', why: (r.reason && whyOf[r.reason]) || (r.answer ? 'side.err.said' : 'side.err.failed'), said: r.answer, ms: r.duration_ms, cost: r.cost_usd })
    },
    (e) => put(key, { ...a, state: 'failed', why: (e instanceof ApiError && whyOf[e.code]) || 'side.err.failed' }),
  )
}

function useKept(key: string): Asked[] {
  return useSyncExternalStore(
    (f) => (heard.add(f), () => heard.delete(f)),
    () => kept.get(key) ?? NONE,
  )
}
const NONE: Asked[] = []

const costSaid = (c: number) => (c < 0.01 ? c.toFixed(4) : c.toFixed(2))

export function SideQuestion({ sid, bind, model, onClose }: { sid: string; bind: SideBind; model?: string; onClose: () => void }) {
  usePrefs() // (its colours follow the theme)
  const key = keyOf(sid, bind)
  const list = useKept(key)
  const [text, setText] = useState('')
  const asking = list.some((a) => a.state === 'asking')
  // (How long the one under way has waited, by the second.)
  const [, tick] = useState(0)
  useEffect(() => {
    if (!asking) return
    const i = window.setInterval(() => tick((n) => n + 1), 1000)
    return () => window.clearInterval(i)
  }, [asking])
  const body = useRef<HTMLDivElement>(null)
  // (Each new one, or its answer, in sight: the list's end.)
  const last = list[list.length - 1]
  useEffect(() => {
    const el = body.current
    if (el) el.scrollTop = el.scrollHeight
  }, [last?.id, last?.state])
  const q = text.trim()
  const long = [...q].length > SIDE_MAX
  const can = !!q && !long && !asking
  const send = () => {
    if (!can) return
    ask(sid, bind, model, q)
    setText('')
  }
  const sendKey = onMac() ? '⌘↩' : 'Ctrl+Enter'
  return (
    <Dialog title={t('side.title')} onClose={onClose} wide hideTitle className="dlg side-dialog">
      <DialogHead icon={<MessageCircleQuestion size={18} />} title={t('side.title')} oneLine onClose={onClose} />
      <div className="dlg-body side-body" ref={body}>
        <p className="side-note">{t('side.note')}</p>
        {list.map((a) => (
          <div key={a.id} className="side-qa">
            <p className="side-q">{a.q}</p>
            {a.state === 'asking' ? (
              <p className="side-wait" role="status">
                <span className="convo-spin" aria-hidden="true" />
                {t('side.asking', { s: Math.max(0, Math.floor((Date.now() - a.at) / 1000)) })}
              </p>
            ) : a.state === 'ok' ? (
              <>
                {/* (In the thread's colours for its code, as a file's preview draws Markdown.) */}
                <div className={'side-a convo-md term-vars ' + (isDark() ? 'dark' : 'light')}>
                  <Markdown text={a.answer ?? ''} soft />
                </div>
                <div className="side-meta">
                  {a.ms ? <span>{t('side.took', { s: (a.ms / 1000).toFixed(1) })}</span> : null}
                  {a.cost ? <span>{t('side.cost', { c: costSaid(a.cost) })}</span> : null}
                  <span className="spacer" />
                  <CopyButton text={a.answer ?? ''} />
                </div>
              </>
            ) : (
              <div className="side-failed" role="alert">
                <span>{t(a.why ?? 'side.err.failed', { why: a.said ?? '' })}</span>
                <button type="button" className="btn" disabled={asking} onClick={() => ask(sid, bind, model, a.q, a.id)}>
                  <RotateCw size={14} aria-hidden="true" />
                  {t('common.retry')}
                </button>
              </div>
            )}
          </div>
        ))}
        {list.length > 0 && <p className="side-kept">{t('side.kept')}</p>}
      </div>
      <div className="dlg-foot side-foot">
        <textarea
          className="input side-input"
          rows={3}
          value={text}
          aria-label={t('side.question')}
          placeholder={t('side.placeholder')}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.ctrlKey || e.metaKey) && !e.nativeEvent.isComposing) {
              e.preventDefault()
              send()
            }
          }}
        />
        {long && <p className="side-long" role="alert">{t('side.tooLong', { n: SIDE_MAX })}</p>}
        <button type="button" className="btn btn-primary side-send" disabled={!can} onClick={send} title={t('side.askKey', { key: sendKey })}>
          {t('side.ask')}
        </button>
      </div>
    </Dialog>
  )
}
