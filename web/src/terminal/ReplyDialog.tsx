import { useEffect, useLayoutEffect, useRef, useState, type FormEvent } from 'react'
import { RotateCw } from 'lucide-react'
import { api, ApiError, errorText, type HerdrAgent } from '../api'
import { t } from '../i18n'
import { Dialog, LoadError, useToast } from '../components/ui'
import { getReplyDraft, keepReplyDraft } from './drafts'
import { choices, question } from './menu'

interface Screen {
  text: string
  digest: string
}

// The last lines of a pane's screen are what the user needs to answer.
const shownLines = 40


// What answering an agent in a herdr pane takes: its screen as it is now,
// the choices on it, and sending keys or a line of text, which goes to the
// pane only if the screen is still the one shown.
// onLoaded: the screen has been shown (the request to answer has been seen).
// resizedAt: when the terminal's size last changed; herdr then redraws the
// pane, so the screen is read again a moment later.
// onSent: after an answer went through.
export function useReply(sessionId: string, agent: HerdrAgent, { onLoaded, resizedAt, onSent }: { onLoaded?: () => void; resizedAt?: number; onSent: () => void }) {
  const toast = useToast()
  const base = `/api/sessions/${sessionId}/herdr/panes/${encodeURIComponent(agent.pane_id)}`
  const [screen, setScreen] = useState<Screen | null>(null)
  const [failed, setFailed] = useState(false)
  const [reading, setReading] = useState(false) // the screen is being read (again)
  const [changed, setChanged] = useState<'' | 'changed' | 'answered'>('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // What is typed stays with the pane while the dialog is closed or its tab
  // is in the back (in memory, as the composer's drafts).
  const draftKey = sessionId + ' ' + agent.pane_id
  const [text, setTextState] = useState(() => getReplyDraft(draftKey))
  const setText = (v: string) => {
    setTextState(v)
    keepReplyDraft(draftKey, v)
  }
  const seq = useRef(0)
  const live = useRef(true) // an answer that comes back after closing is not shown
  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
    }
  }, [])
  const loadedRef = useRef(onLoaded)
  loadedRef.current = onLoaded

  const load = async () => {
    const n = ++seq.current
    setFailed(false)
    setReading(true)
    try {
      const s = await api<Screen>('GET', base + '/screen')
      if (n !== seq.current) return
      loadedRef.current?.()
      setScreen(s)
      setChanged('')
      setError('')
    } catch {
      if (n === seq.current) setFailed(true)
    } finally {
      if (n === seq.current && live.current) setReading(false)
    }
  }
  useEffect(() => {
    void load()
    return () => {
      seq.current++
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [base])
  const busyRef = useRef(false)
  busyRef.current = busy
  const firstSize = useRef(resizedAt)
  useEffect(() => {
    if (!resizedAt || resizedAt === firstSize.current) return
    const tm = window.setTimeout(() => {
      if (!busyRef.current) void load()
    }, 800)
    return () => clearTimeout(tm)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [resizedAt])

  const send = async (body: { keys: string[] } | { text: string }) => {
    if (!screen || busy) return
    setBusy(true)
    setError('')
    seq.current++ // a refresh still on its way must not replace what this answer meets
    setReading(false)
    try {
      await api('POST', base + '/reply', { digest: screen.digest, ...body })
      // The text sent goes from what is kept for the pane (not a newer one
      // typed while this answer was on its way).
      if ('text' in body && getReplyDraft(draftKey).trim() === body.text) keepReplyDraft(draftKey, '')
      toast(t('reply.sent'))
      if (live.current) onSent()
    } catch (e) {
      if (!live.current) return
      if (e instanceof ApiError && (e.code === 'screen_changed' || e.code === 'already_answered')) {
        setScreen({ text: String(e.extra.text ?? ''), digest: String(e.extra.digest ?? '') })
        setChanged(e.code === 'already_answered' ? 'answered' : 'changed')
      } else setError(e instanceof ApiError && e.code === 'herdr_busy' ? t('herdr.busy') : errorText(e))
    } finally {
      if (live.current) setBusy(false)
    }
  }
  const submit = (e: FormEvent) => {
    e.preventDefault()
    const v = text.trim()
    if (v) void send({ text: v })
  }
  const opts = screen ? choices(screen.text) : []
  return { screen, failed, reading, changed, busy, error, text, setText, load, send, submit, opts }
}


// Answer an agent in a herdr pane: its screen as it is now, the choices on
// it, Enter, Esc, or a line of text (see useReply).
export function ReplyDialog({ sessionId, agent, onLoaded, onClose, resizedAt }: { sessionId: string; agent: HerdrAgent; onLoaded?: () => void; onClose: () => void; resizedAt?: number }) {
  const { screen, failed, reading, changed, busy, error, text, setText, load, send, submit, opts } = useReply(sessionId, agent, { onLoaded, resizedAt, onSent: onClose })
  const pre = useRef<HTMLPreElement>(null)
  // The end of the screen is where the question is.
  useLayoutEffect(() => {
    if (pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [screen])

  const shown = screen ? screen.text.split('\n').slice(-shownLines).join('\n') : ''
  const title = agent.agent || t('reply.terminal')
  return (
    <Dialog title={t('reply.title', { agent: title })} onClose={onClose} className="reply-dialog">
      {agent.title && <p className="hint reply-sub">{agent.title}</p>}
      {failed ? (
        <LoadError onRetry={() => void load()} />
      ) : (
        <>
          <div className="reply-screen-wrap">
            <pre className="reply-screen" ref={pre} tabIndex={0} aria-label={t('reply.screen')} aria-busy={reading}>
              {screen ? shown || t('reply.blank') : t('common.loading')}
            </pre>
            <button type="button" className={'icon-btn reply-refresh' + (reading ? ' reading' : '')} onClick={() => void load()} disabled={busy || reading} aria-label={t('reply.refresh')} title={t('reply.refresh')}>
              <RotateCw size={16} />
            </button>
          </div>
          {changed && (
            <p className="reply-changed" role="alert">
              {t(changed === 'answered' ? 'reply.answered' : 'reply.changed')}
            </p>
          )}
          {opts.length > 0 && (
            <div className="reply-choices" role="group" aria-label={t('reply.choices')}>
              {opts.map((o) => (
                <button key={o.key} type="button" className="btn reply-choice" disabled={!screen || busy} onClick={() => void send({ keys: [o.key] })}>
                  <span className="reply-key">{o.key}</span>
                  <span className="reply-label">{o.label}</span>
                </button>
              ))}
            </div>
          )}
          <div className="reply-keys">
            <button type="button" className="btn" disabled={!screen || busy} onClick={() => void send({ keys: ['enter'] })}>
              {t('reply.enter')}
            </button>
            <button type="button" className="btn" disabled={!screen || busy} onClick={() => void send({ keys: ['esc'] })}>
              {t('reply.esc')}
            </button>
          </div>
          <form className="reply-text" onSubmit={submit}>
            <input
              className="input"
              value={text}
              maxLength={2000}
              placeholder={t('reply.placeholder')}
              aria-label={t('reply.placeholder')}
              autoCapitalize="none"
              autoCorrect="off"
              onChange={(e) => setText(e.target.value)}
              data-touch-nofocus
            />
            <button className="btn btn-primary" disabled={!screen || busy || !text.trim()}>
              {t('reply.send')}
            </button>
          </form>
          {error && (
            <p className="err" role="alert">
              {error}
            </p>
          )}
        </>
      )}
      <div className="dialog-actions">
        <button className="btn" onClick={onClose}>
          {t('common.close')}
        </button>
      </div>
    </Dialog>
  )
}

// The same, in the conversation: the question and its choices at the end of
// the thread, a line of text, and the screen itself on demand.
export function InlineReply({ sessionId, agent, onLoaded, resizedAt }: { sessionId: string; agent: HerdrAgent; onLoaded?: () => void; resizedAt?: number }) {
  const [sent, setSent] = useState(false)
  const [shown, setShown] = useState(false)
  const { screen, failed, reading, changed, busy, error, text, setText, load, send, submit, opts } = useReply(sessionId, agent, { onLoaded, resizedAt, onSent: () => setSent(true) })
  const pre = useRef<HTMLPreElement>(null)
  useLayoutEffect(() => {
    if (pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [screen, shown])
  if (sent) return <p className="convo-ask-sent" role="status">{t('convo.answeredHere')}</p>
  const q = screen ? question(screen.text) : ''
  return (
    <div className="convo-ask" role="group" aria-label={t('convo.waiting')}>
      <div className="convo-ask-q">{q || t('convo.waiting')}</div>
      {failed ? (
        <LoadError onRetry={() => void load()} />
      ) : (
        <>
          {changed && (
            <p className="reply-changed" role="alert">
              {t(changed === 'answered' ? 'reply.answered' : 'reply.changed')}
            </p>
          )}
          {opts.length > 0 && (
            <div className="convo-ask-opts" role="group" aria-label={t('reply.choices')}>
              {opts.map((o) => (
                <button key={o.key} type="button" className="convo-ask-opt" disabled={!screen || busy} onClick={() => void send({ keys: [o.key] })}>
                  <span className="convo-ask-key">{o.key}</span>
                  <span>{o.label}</span>
                </button>
              ))}
            </div>
          )}
          <form className="convo-ask-text" onSubmit={submit}>
            <input
              className="input"
              value={text}
              maxLength={2000}
              placeholder={t('reply.placeholder')}
              aria-label={t('reply.placeholder')}
              autoCapitalize="none"
              autoCorrect="off"
              onChange={(e) => setText(e.target.value)}
            />
            <button className="btn" disabled={!screen || busy || !text.trim()}>
              {t('reply.send')}
            </button>
          </form>
          <div className="convo-ask-more">
            <button type="button" className="link-btn" aria-expanded={shown} onClick={() => setShown(!shown)}>
              {t(shown ? 'convo.hideScreen' : 'convo.showScreen')}
            </button>
            {opts.length === 0 && (
              <button type="button" className="link-btn" disabled={!screen || busy} onClick={() => void send({ keys: ['enter'] })}>
                {t('reply.enter')}
              </button>
            )}
            <button type="button" className="link-btn" disabled={!screen || busy} onClick={() => void send({ keys: ['esc'] })}>
              {t('reply.esc')}
            </button>
          </div>
          {shown && (
            <div className="reply-screen-wrap">
              <pre className="reply-screen" ref={pre} tabIndex={0} aria-label={t('reply.screen')} aria-busy={reading}>
                {screen ? screen.text.split('\n').slice(-shownLines).join('\n') || t('reply.blank') : t('common.loading')}
              </pre>
              <button type="button" className={'icon-btn reply-refresh' + (reading ? ' reading' : '')} onClick={() => void load()} disabled={busy || reading} aria-label={t('reply.refresh')} title={t('reply.refresh')}>
                <RotateCw size={16} />
              </button>
            </div>
          )}
          {error && (
            <p className="err" role="alert">
              {error}
            </p>
          )}
        </>
      )}
    </div>
  )
}
