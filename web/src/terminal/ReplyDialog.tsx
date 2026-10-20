import { useEffect, useLayoutEffect, useRef, useState, type FormEvent } from 'react'
import { RotateCw } from 'lucide-react'
import { api, ApiError, errorText, type HerdrAgent } from '../api'
import { t } from '../i18n'
import { Dialog, LoadError, useToast } from '../components/ui'
import { getReplyDraft, keepReplyDraft } from './drafts'

interface Screen {
  text: string
  digest: string
}

// The last lines of a pane's screen are what the user needs to answer.
const shownLines = 40

// choices finds the menu an agent asks with at the end of the screen, as
// Claude Code, Codex and Gemini CLI draw it: numbered lines in a row, one of
// them marked as selected ("❯ 1. Yes", "  2. No"), followed by at most a few
// lines of blank space, borders or key hints ("Esc to cancel"). The number
// is the key that picks a choice. A numbered list without the mark
// (ordinary output), or one followed by anything else (a new question), is
// no menu.
const menuLine = /^[\s│|]*([❯›>▶→●◉])?\s*([1-9])[.)]\s+(\S.*?)\s*[│|]?\s*$/
const hintLine = /^[\s│|╭╮╰╯─━┌┐└┘]*$|\b(esc|cancel|confirm)\b/i
const maxLabel = 120

export function choices(text: string): { key: string; label: string }[] {
  const lines = text.split('\n')
  let end = lines.length
  while (end > 0 && !menuLine.test(lines[end - 1]) && hintLine.test(lines[end - 1]) && lines.length - end < 3) end--
  const out: { key: string; label: string }[] = []
  let marked = 0
  for (let i = end - 1; i >= 0; i--) {
    const m = menuLine.exec(lines[i])
    if (!m) break
    out.unshift({ key: m[2], label: m[3].length > maxLabel ? m[3].slice(0, maxLabel - 1) + '…' : m[3] })
    if (m[1]) marked++
  }
  const inOrder = out.every((c, i) => c.key === String(i + 1))
  return out.length >= 2 && inOrder && marked === 1 ? out : []
}

// Answer an agent in a herdr pane: its screen as it is now, the choices on
// it, Enter, Esc, or a line of text. What is sent goes to that pane only if
// the screen is still the one shown.
// onLoaded: the screen has been shown (the request to answer has been seen).
// resizedAt: when the terminal's size last changed; herdr then redraws the
// pane, so the screen is read again a moment later.
export function ReplyDialog({ sessionId, agent, onLoaded, onClose, resizedAt }: { sessionId: string; agent: HerdrAgent; onLoaded?: () => void; onClose: () => void; resizedAt?: number }) {
  const toast = useToast()
  const base = `/api/sessions/${sessionId}/herdr/panes/${encodeURIComponent(agent.pane_id)}`
  const [screen, setScreen] = useState<Screen | null>(null)
  const [failed, setFailed] = useState(false)
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
  const pre = useRef<HTMLPreElement>(null)
  const seq = useRef(0)
  const live = useRef(true) // an answer that comes back after closing is not shown
  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
    }
  }, [])

  const load = async () => {
    const n = ++seq.current
    setFailed(false)
    try {
      const s = await api<Screen>('GET', base + '/screen')
      if (n !== seq.current) return
      onLoaded?.()
      setScreen(s)
      setChanged('')
      setError('')
    } catch {
      if (n === seq.current) setFailed(true)
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
  // The end of the screen is where the question is.
  useLayoutEffect(() => {
    if (pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [screen])

  const send = async (body: { keys: string[] } | { text: string }) => {
    if (!screen || busy) return
    setBusy(true)
    setError('')
    seq.current++ // a refresh still on its way must not replace what this answer meets
    try {
      await api('POST', base + '/reply', { digest: screen.digest, ...body })
      // The text sent goes from what is kept for the pane (not a newer one
      // typed while this answer was on its way).
      if ('text' in body && getReplyDraft(draftKey).trim() === body.text) keepReplyDraft(draftKey, '')
      toast(t('reply.sent'))
      if (live.current) onClose()
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

  const shown = screen ? screen.text.split('\n').slice(-shownLines).join('\n') : ''
  const opts = screen ? choices(screen.text) : []
  const title = agent.agent || t('reply.terminal')
  return (
    <Dialog title={t('reply.title', { agent: title })} onClose={onClose} className="reply-dialog">
      {agent.title && <p className="hint reply-sub">{agent.title}</p>}
      {failed ? (
        <LoadError onRetry={() => void load()} />
      ) : (
        <>
          <div className="reply-screen-wrap">
            <pre className="reply-screen" ref={pre} tabIndex={0} aria-label={t('reply.screen')}>
              {screen ? shown || t('reply.blank') : t('common.loading')}
            </pre>
            <button type="button" className="icon-btn reply-refresh" onClick={() => void load()} disabled={busy} aria-label={t('reply.refresh')} title={t('reply.refresh')}>
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
