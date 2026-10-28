import { useEffect, useLayoutEffect, useRef, useState, type FormEvent } from 'react'
import { RotateCw } from 'lucide-react'
import { api, ApiError, errorText, type HerdrAgent } from '../api'
import { t } from '../i18n'
import { Dialog, isCoarse, LoadError, useToast } from '../components/ui'
import { getReplyDraft, keepReplyDraft } from './drafts'
import { asksQuestion, choices, dashedRule, promptBlock, question } from './menu'
import { choiceText, lasting, questionText } from './askText'

interface Screen {
  text: string
  digest: string
  cut?: boolean // (its start was cut: longer than the gateway keeps)
}

// folds: the screen may go behind a button, its choices all there is to
// it: a question of Claude Code's, its question read whole, the screen not
// cut. Anything else, what it asks shows (as read, else the screen itself).
function folds(screen: Screen): boolean {
  if (screen.cut || !asksQuestion(screen.text)) return false
  const q = question(screen.text)
  // (A choice, or what it is, too long to be shown whole: the screen too.)
  const cutShort = choices(screen.text).some((c) => c.label.endsWith('…') || !!c.desc?.endsWith('…'))
  return !!q && !q.startsWith('…') && !cutShort
}

const isTouchOnly = isCoarse()

// The last lines of a pane's screen are what the user needs to answer.
const shownLines = 40


// Where an answer goes: a herdr pane (herdr's own answer), or the process a
// conversation shows (tmux too: the gateway checks on the host that it
// still waits, on the screen shown). key: what it is known by (drafts,
// and what is read anew when it changes).
export interface ReplyTarget {
  key: string
  screen: () => Promise<Screen>
  reply: (body: { digest: string } & ({ keys: string[] } | { text: string })) => Promise<unknown>
}

export function herdrTarget(sessionId: string, agent: HerdrAgent): ReplyTarget {
  const base = `/api/sessions/${sessionId}/herdr/panes/${encodeURIComponent(agent.pane_id)}`
  return { key: sessionId + ' ' + agent.pane_id, screen: () => api<Screen>('GET', base + '/screen'), reply: (b) => api('POST', base + '/reply', b) }
}

export function convoTarget(sessionId: string, bind: { pane: string; pid: number; start: number; session: string }): ReplyTarget {
  const base = `/api/sessions/${sessionId}/conversation`
  const who = { pane: bind.pane, pid: bind.pid, start: bind.start, session: bind.session }
  return {
    key: [sessionId, bind.pane, bind.pid, bind.start, bind.session].join(' '),
    screen: () => api<Screen>('POST', base + '/screen', who),
    reply: (b) => api('POST', base + '/reply', { ...who, ...b }),
  }
}

// What answering an agent takes: its screen as it is now, the choices on
// it, and sending keys or a line of text, which goes to the pane only if
// the screen is still the one shown.
// onLoaded: the screen has been shown (the request to answer has been seen).
// resizedAt: when the terminal's size last changed; herdr then redraws the
// pane, so the screen is read again a moment later.
// onSent: after an answer went through.
export function useReply(target: ReplyTarget, { onLoaded, resizedAt, onSent }: { onLoaded?: () => void; resizedAt?: number; onSent: () => void }) {
  const toast = useToast()
  const base = target.key
  const targetRef = useRef(target)
  targetRef.current = target
  const [screen, setScreen] = useState<Screen | null>(null)
  const [failed, setFailed] = useState(false)
  const [reading, setReading] = useState(false) // the screen is being read (again)
  const [changed, setChanged] = useState<'' | 'changed' | 'answered'>('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // What is typed stays with the pane while the dialog is closed or its tab
  // is in the back (in memory, as the composer's drafts).
  const draftKey = target.key
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
      const s = await targetRef.current.screen()
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
      await targetRef.current.reply({ digest: screen.digest, ...body })
      // The text sent goes from what is kept for the pane (not a newer one
      // typed while this answer was on its way).
      if ('text' in body && getReplyDraft(draftKey).trim() === body.text) {
        keepReplyDraft(draftKey, '')
        // (Shown too, not only kept: the next question here starts empty.)
        if (live.current) setTextState((v) => (v.trim() === body.text ? '' : v))
      }
      toast(t('reply.sent'))
      if (live.current) onSent()
    } catch (e) {
      if (!live.current) return
      if (e instanceof ApiError && (e.code === 'screen_changed' || e.code === 'already_answered')) {
        // (herdr's answer says the screen as it is now; the conversation's is read anew.)
        if (typeof e.extra.digest === 'string') setScreen({ text: String(e.extra.text ?? ''), digest: e.extra.digest, cut: e.extra.cut === true })
        else void load().then(() => live.current && setChanged(e.code === 'already_answered' ? 'answered' : 'changed'))
        setChanged(e.code === 'already_answered' ? 'answered' : 'changed')
      } else if (e instanceof ApiError && e.code === 'not_waiting') setError(t('reply.notWaiting'))
      else if (e instanceof ApiError && e.code === 'cannot_verify') setError(t('reply.cannotVerify'))
      else setError(e instanceof ApiError && (e.code === 'herdr_busy' || e.code === 'busy') ? t('herdr.busy') : errorText(e))
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
  const { screen, failed, reading, changed, busy, error, text, setText, load, send, submit, opts } = useReply(herdrTarget(sessionId, agent), { onLoaded, resizedAt, onSent: onClose })
  const confirm = useConfirm(screen, opts, (keys) => void send({ keys }))
  const pre = useRef<HTMLPreElement>(null)
  const [screenOpen, setScreenOpen] = useState(false) // (a menu read off it: the screen behind a button)
  // The end of the screen is where the question is; where the screen is
  // all that says what is asked, its start is where that begins.
  useLayoutEffect(() => {
    if (pre.current) pre.current.scrollTop = whole ? 0 : pre.current.scrollHeight
  }, [screen, screenOpen])

  // (What is asked not read off it, all of it: a long patch's start is above its last lines.)
  const whole = !!screen && !promptBlock(screen.text) && !folds(screen)
  const shown = screen ? (whole ? screen.text : screen.text.split('\n').slice(-shownLines).join('\n')) : ''
  const title = agent.agent || t('reply.terminal')
  const q = screen ? question(screen.text) : ''
  // (Only a question of Claude Code's is all its choices: anything else, what
  // it asks to be allowed shows first, as read, else the screen.)
  const block = screen ? promptBlock(screen.text) : null
  const questionOnly = !!screen && folds(screen)
  return (
    <Dialog title={t('reply.title', { agent: title })} onClose={onClose} className="reply-dialog">
      {agent.title && <p className="hint reply-sub">{agent.title}</p>}
      {failed ? (
        <LoadError onRetry={() => void load()} />
      ) : (
        <>
          {/* (A menu read off it: its question and choices first, the screen
              behind a button; else the screen, to answer from.) */}
          {screen?.cut && <p className="reply-changed" role="alert">{t('reply.cut')}</p>}
          {opts.length > 0 && q && <p className="reply-q">{block && /^Bash command$/i.test(block.head) && /^Do you want to proceed\?$/i.test(q) ? t('convo.askRun') : questionText(q)}</p>}
          {/* (What a permission asks to be allowed, before its choices.) */}
          {opts.length > 0 && block && <Asked lines={block.lines} />}
          {(opts.length === 0 || screenOpen || (!block && !questionOnly)) && (
            <div className="reply-screen-wrap">
              <pre className="reply-screen" ref={pre} tabIndex={0} aria-label={t('reply.screen')} aria-busy={reading}>
                {screen ? shown || t('reply.blank') : t('common.loading')}
              </pre>
              <button type="button" className={'icon-btn reply-refresh' + (reading ? ' reading' : '')} onClick={() => void load()} disabled={busy || reading} aria-label={t('reply.refresh')} title={t('reply.refresh')}>
                <RotateCw size={16} />
              </button>
            </div>
          )}
          {changed && (
            <p className="reply-changed" role="alert">
              {t(changed === 'answered' ? 'reply.answered' : 'reply.changed')}
            </p>
          )}
          {opts.length > 0 && (
            <div className="reply-choices" role="group" aria-label={t('reply.choices')}>
              {opts.map((o) => {
                const c = choiceText(o.label)
                const lasts = lasting(o.label)
                return (
                  <button key={o.key} type="button" className={'reply-choice' + (confirm.asked === o.key ? ' is-confirm' : '')} disabled={!screen || busy} onClick={() => confirm.choose(o.key)}>
                    <span className="reply-key" aria-hidden="true">{o.key}</span>
                    {/* (Its words as Claude Code wrote them, where shown in Chinese: on a pointer's rest.) */}
                    <span className="reply-what" title={c.original || undefined}>
                      <span className="reply-label">{c.text}</span>
                      {o.desc && <span className="reply-desc">{o.desc}</span>}
                      {lasts && <span className="reply-desc is-lasting">{t(confirm.asked === o.key ? 'convo.askConfirm' : lasts === 'rule' ? 'convo.askRule' : 'convo.askMode')}</span>}
                    </span>
                  </button>
                )
              })}
            </div>
          )}
          <form className="reply-text" onSubmit={submit}>
            <input
              className="input"
              value={text}
              maxLength={2000}
              placeholder={t(opts.length > 0 ? 'reply.other' : 'reply.placeholder')}
              aria-label={t(opts.length > 0 ? 'reply.other' : 'reply.placeholder')}
              autoCapitalize="none"
              autoCorrect="off"
              onChange={(e) => setText(e.target.value)}
              data-touch-nofocus
            />
            <button className="btn btn-primary" disabled={!screen || busy || !text.trim()}>
              {t('reply.send')}
            </button>
          </form>
          <div className="reply-keys">
            <button type="button" className={'btn btn-ghost btn-sm' + (confirm.asked === 'enter' ? ' is-confirm' : '')} disabled={!screen || busy} onClick={confirm.enter}>
              {t(confirm.asked === 'enter' ? 'reply.enterAgain' : 'reply.enter')}
            </button>
            <button type="button" className="btn btn-ghost btn-sm" disabled={!screen || busy} onClick={() => void send({ keys: ['esc'] })}>
              {t('reply.esc')}
            </button>
            {opts.length > 0 && (block || questionOnly) && (
              <button type="button" className="btn btn-ghost btn-sm reply-show" aria-expanded={screenOpen} onClick={() => setScreenOpen((v) => !v)}>
                {t(screenOpen ? 'reply.hideScreen' : 'reply.showScreen')}
              </button>
            )}
          </div>
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

// What the agent asks to be allowed, as its screen shows it (see
// A choice that lasts beyond this one (a rule kept, the session's mode
// changed) goes on a second tap or key, within a few seconds of the first:
// one tap does not set what goes on after. asked: the choice waiting for
// its second.
// (Of this screen only: another screen's choice of the same number is
// another choice, asked anew. Enter takes the choice marked on the screen,
// so it asks as that one does.)
function useConfirm(screen: { text: string; digest: string } | null, opts: { key: string; label: string }[], send: (keys: string[]) => void) {
  const [asked, setAsked] = useState<{ key: string; digest: string } | null>(null)
  const digest = screen?.digest ?? ''
  useEffect(() => {
    if (!asked) return
    const tm = window.setTimeout(() => setAsked(null), 6000)
    return () => clearTimeout(tm)
  }, [asked])
  const choose = (key: string, keys = [key]) => {
    const o = opts.find((x) => x.key === key)
    if (o && lasting(o.label) && !(asked?.key === key && asked.digest === digest)) return setAsked({ key, digest })
    setAsked(null)
    send(keys)
  }
  // Enter: the choice marked ("❯ 2."), the last marked on the screen (the
  // menu is at its end; one before it is of earlier output). None told,
  // where a choice lasts, Enter asks for its second too.
  const enter = () => {
    const marks = screen ? [...screen.text.matchAll(/^[\s│┃|]*❯\s*(\d)[.)]/gm)] : []
    const m = marks.length ? marks[marks.length - 1][1] : null
    if (m && opts.some((o) => o.key === m)) return choose(m, ['enter'])
    if (opts.some((o) => lasting(o.label)) && !(asked?.key === 'enter' && asked.digest === digest)) return setAsked({ key: 'enter', digest })
    setAsked(null)
    send(['enter'])
  }
  return { asked: asked && asked.digest === digest ? asked.key : null, choose, enter }
}

// promptBlock): an edit's lines marked added and removed as the screen marks
// them ("12 +", "12 -", and the lines going on from them).
function Asked({ lines }: { lines: string[] }) {
  let sign: '' | 'add' | 'del' = ''
  return (
    <pre className="convo-pre convo-ask-block">
      {lines.map((l, i) => {
        const m = /^\s*\d+\s?([-+])/.exec(l)
        if (m) sign = m[1] === '+' ? 'add' : 'del'
        else if (!(sign && /^\s+[-+]/.test(l))) sign = ''
        return (
          <span key={i} className={sign ? 'convo-' + sign : dashedRule.test(l) ? 'convo-ask-rule' : undefined}>
            {l + '\n'}
          </span>
        )
      })}
    </pre>
  )
}

// Keys that answer the card while it shows and no field has them: a
// choice's number, Esc for no. (The terminal's own field counts as none:
// the key is the card's, not the pane's.) While an answer is on its way, or
// sent, or a key is held, such keys go nowhere: not on to the pane, where
// they would answer what comes next without the screen being checked.
// Not with a modifier (the key bar's latched ones too: held), while an input
// method composes, or while a menu or dialog is open (Esc is its).
function useAnswerKeys(on: boolean, ready: boolean, keys: string[], held: (() => boolean) | undefined, card: { current: HTMLElement | null }, onKey: (k: string) => void) {
  const ref = useRef(onKey)
  ref.current = onKey
  const heldRef = useRef(held)
  heldRef.current = held
  const readyRef = useRef(ready)
  readyRef.current = ready
  const list = keys.join(' ')
  useEffect(() => {
    if (!on) return
    const ks = list.split(' ')
    const handle = (e: KeyboardEvent) => {
      if (e.ctrlKey || e.metaKey || e.altKey || e.isComposing || e.keyCode === 229 || heldRef.current?.()) return
      const el = e.target as HTMLElement | null
      const typing = !!el && !el.classList.contains('xterm-helper-textarea') && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable)
      // (The changes panel open over this conversation: it may cover the card, and Esc is its.)
      if (typing || document.querySelector('[aria-modal="true"], [role="menu"], [role="dialog"]') || card.current?.closest('.convo')?.querySelector('.convo-panel')) return
      // (Only with the card in hand, its focus in it: not a key pressed for
      // the page, the card out of sight.)
      if (card.current && !card.current.contains(document.activeElement)) return
      const k = e.key === 'Escape' ? 'esc' : e.key
      if (!ks.includes(k) && !/^[0-9]$/.test(k)) return
      e.preventDefault()
      e.stopPropagation()
      if (!e.repeat && readyRef.current && ks.includes(k)) ref.current(k)
    }
    window.addEventListener('keydown', handle, true)
    return () => window.removeEventListener('keydown', handle, true)
  }, [on, list])
}

// The same, in the conversation, over the composer: the question, what it is
// of (from the log), its choices (keys 1–9, Esc), a line of text, and the
// screen itself on demand.
export function InlineReply({ target, onLoaded, resizedAt, active = true, held }: { target: ReplyTarget; onLoaded?: () => void; resizedAt?: number; active?: boolean; held?: () => boolean }) {
  const [sent, setSent] = useState<string | null>(null) // (the screen answered)
  const [shown, setShown] = useState(false)
  const answered = useRef('')
  const { screen, failed, reading, changed, busy, error, text, setText, load, send, submit, opts } = useReply(target, { onLoaded, resizedAt, onSent: () => setSent(answered.current) })
  if (screen) answered.current = screen.digest
  // Answered, it is read again now and then (each read after the last came
  // back) while the agent still waits: another question in the same pane,
  // before the wait was seen to end, shows instead of the note: at once with
  // choices, else once the screen has stayed other than the one answered
  // (an agent going on is seen to work before that).
  const loadRef = useRef(load)
  loadRef.current = load
  const activeRef = useRef(active)
  activeRef.current = active
  useEffect(() => {
    if (sent === null) return
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    // (Hidden, or behind a terminal tab, it is not read: once shown again, it is, at the next turn.)
    const next = () => {
      timer = setTimeout(() => (document.hidden || !activeRef.current ? next() : void loadRef.current().finally(() => !stopped && next())), 2000)
    }
    next()
    return () => {
      stopped = true
      clearTimeout(timer)
    }
  }, [sent])
  const otherReads = useRef(0)
  useEffect(() => {
    if (sent === null || !screen) return
    otherReads.current = screen.digest === sent ? 0 : otherReads.current + 1
    if (otherReads.current > 0 && (opts.length > 0 || otherReads.current >= 2)) {
      otherReads.current = 0
      setSent(null)
    }
  }, [sent, screen, opts.length])
  const pre = useRef<HTMLPreElement>(null)
  const block = screen ? promptBlock(screen.text) : null
  // (What is asked not told apart on the screen, the screen itself shows; a
  // question of Claude Code's is all its choices.)
  const needScreen = !!screen && !block && !folds(screen)
  // (Not told apart, the whole screen, before the choices; else behind its button.)
  const screenLines = screen ? screen.text.split('\n') : []
  const screenBox = (
            // (What is asked could not be told apart on the screen: the screen itself.)
            <div className="reply-screen-wrap">
              <pre className="reply-screen" ref={pre} tabIndex={0} aria-label={t('reply.screen')} aria-busy={reading}>
                {screen ? (needScreen ? screenLines : screenLines.slice(-shownLines)).join('\n') || t('reply.blank') : t('common.loading')}
              </pre>
              <button type="button" className={'icon-btn reply-refresh' + (reading ? ' reading' : '')} onClick={() => void load()} disabled={busy || reading} aria-label={t('reply.refresh')} title={t('reply.refresh')}>
                <RotateCw size={16} />
              </button>
            </div>
  )
  useLayoutEffect(() => {
    if (pre.current) pre.current.scrollTop = needScreen ? 0 : pre.current.scrollHeight
  }, [screen, shown, !block])
  // Keys answer only the screen the card was taken in hand on (touched,
  // or its focus come in): another screen, read meanwhile, is looked at
  // again (touched again) before a key answers it.
  const [armedFor, setArmedFor] = useState('')
  const arm = () => setArmedFor(screen?.digest ?? '')
  const confirm = useConfirm(screen, opts, (keys) => void send({ keys }))
  const ready = sent === null && !!screen && !busy && !failed && armedFor === screen.digest
  const cardRef = useRef<HTMLDivElement>(null)
  useAnswerKeys(active, ready, [...opts.map((o) => o.key), 'esc'], held, cardRef, (k) => (k === 'esc' ? void send({ keys: [k] }) : confirm.choose(k)))
  if (sent !== null) return <p className="convo-ask-sent" role="status">{t('convo.answeredHere')}</p>
  const q = screen ? question(screen.text) : ''
  const shownQ = block && /^Bash command$/i.test(block.head) && /^Do you want to proceed\?$/i.test(q) ? t('convo.askRun') : questionText(q)
  return (
    <div className="convo-ask" role="group" aria-label={t('convo.waiting')} ref={cardRef} onPointerDown={arm} onFocus={arm}>
      <div className="convo-ask-q">{shownQ || t('convo.waiting')}</div>
      {block && <Asked lines={block.lines} />}
      {(needScreen || !!screen?.cut) && !failed && <p className="convo-ask-unread">{t(screen?.cut ? 'reply.cut' : 'convo.askUnread')}</p>}
      {failed ? (
        <LoadError onRetry={() => void load()} />
      ) : (
        <>
          {changed && (
            <p className="reply-changed" role="alert">
              {t(changed === 'answered' ? 'reply.answered' : 'reply.changed')}
            </p>
          )}
          {needScreen && screenBox}
          {opts.length > 0 && (
            <div className="convo-ask-opts" role="group" aria-label={t('reply.choices')}>
              {opts.map((o) => {
                const c = choiceText(o.label)
                const lasts = lasting(o.label)
                return (
                  <button key={o.key} type="button" className={'convo-ask-opt' + (confirm.asked === o.key ? ' is-confirm' : '')} disabled={!screen || busy} onClick={() => confirm.choose(o.key)}>
                    <span className="convo-ask-key">{o.key}</span>
                    {/* (Its words as Claude Code wrote them, where shown in Chinese: on a pointer's rest.) */}
                    <span className="convo-ask-what" title={c.original || undefined}>
                      <span className="convo-ask-label">{c.text}</span>
                      {o.desc && <span className="convo-ask-desc">{o.desc}</span>}
                      {lasts && <span className="convo-ask-desc is-lasting">{t(confirm.asked === o.key ? 'convo.askConfirm' : lasts === 'rule' ? 'convo.askRule' : 'convo.askMode')}</span>}
                    </span>
                  </button>
                )
              })}
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
            {!isTouchOnly && (
              <span className="convo-ask-keys">
                {opts.length > 0 ? t('convo.askKeys', { n: opts.length }) : t('convo.askEsc')}
              </span>
            )}
            <button type="button" className="link-btn" aria-expanded={shown || needScreen} disabled={needScreen} onClick={() => setShown(!shown)}>
              {t(shown ? 'convo.hideScreen' : 'convo.showScreen')}
            </button>
            {opts.length === 0 && (
              <button type="button" className="link-btn" disabled={!screen || busy} onClick={confirm.enter}>
                {t('reply.enter')}
              </button>
            )}
            {opts.length === 0 && (
              // (With choices, the last one says no, and Esc does too.)
              <button type="button" className="link-btn" disabled={!screen || busy} onClick={() => void send({ keys: ['esc'] })}>
                {t('reply.esc')}
              </button>
            )}
          </div>
          {shown && !needScreen && screenBox}
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
