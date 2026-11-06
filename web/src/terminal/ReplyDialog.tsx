import { Fragment, useContext, useEffect, useLayoutEffect, useRef, useState, type FormEvent } from 'react'
import { RotateCw } from 'lucide-react'
import { api, ApiError, errorText, type HerdrAgent } from '../api'
import { t } from '../i18n'
import { Dialog, isCoarse, LoadError, useToast } from '../components/ui'
import { getReplyDraft, keepReplyDraft } from './drafts'
import { asksQuestion, choices, codexAsk, codexOptions, dashedRule, promptBlock, question } from './menu'
import { agentName, choiceText, lasting, questionText } from './askText'
import { askLead, readAskScreen, reviewAsDrawn } from './askScreen'
import { PanelWindow } from './PanelWindow'
import { ShowTerminal } from './showTerminal'
import { askOn, AskPanel, OwnBox, planOn, PlanPanel, ReviewPanel, screenCall, useDrive } from './askCard'
import { Filled, MaybeSent, NotAsShown, ownWords, permissionAmend } from './ownWords'
import type { ConvoTool } from './conversation'
import { AskHookHint, asksFromScreen } from './askHook'
import { keepShown, steadily } from './steadyReply'

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

// A screen's lines, its dashed rules each kept to one line (cut where the
// box ends, not wrapped into a stray row of dashes on a narrow screen).
function ScreenText({ lines }: { lines: string[] }) {
  return (
    <>
      {lines.map((l, i) =>
        dashedRule.test(l) ? (
          <span key={i} className="reply-rule">
            {l}
          </span>
        ) : (
          <Fragment key={i}>{l + (i < lines.length - 1 ? '\n' : '')}</Fragment>
        ),
      )}
    </>
  )
}


// Where an answer goes: a herdr pane (herdr's own answer), or the process a
// conversation shows (tmux too: the gateway checks on the host that it
// still waits, on the screen shown). key: what it is known by (drafts,
// and what is read anew when it changes).
export interface ReplyTarget {
  key: string
  agent?: string // (whose: claude, codex…)
  screen: () => Promise<Screen>
  // (alive: the one answering still there — else an answer refused as only
  // a countdown went on is not sent again: steadyReply.ts.)
  // once: not sent again so at all.)
  reply: (body: { digest: string; alive?: () => boolean; once?: boolean } & ({ keys: string[] } | { text: string })) => Promise<unknown>
  // (A text only pasted into a prompt's row that takes words, no Enter:
  // the conversation's only — see ownWords.ts.)
  fill?: (body: { digest: string; text: string }) => Promise<unknown>
  host?: { id: number; name: string } // (the host it runs on, where known: for AskHookHint)
  live?: { sid: string; press: () => Record<string, unknown> } // (Claude Code's in the conversation: its terminal can be shown here: PanelWindow)
}

export function herdrTarget(sessionId: string, agent: HerdrAgent): ReplyTarget {
  const base = `/api/sessions/${sessionId}/herdr/panes/${encodeURIComponent(agent.pane_id)}`
  const screen = async () => keepShown(await api<Screen>('GET', base + '/screen'))
  return { key: sessionId + ' ' + agent.pane_id, agent: agent.agent, screen, reply: ({ alive, once, ...b }) => steadily((x) => api('POST', base + '/reply', x), screen, b, once || agent.agent !== 'claude' ? () => false : alive) }
}

export function convoTarget(sessionId: string, bind: { pane: string; pid: number; start: number; session: string; agent?: string }, canFill = false): ReplyTarget {
  const base = `/api/sessions/${sessionId}/conversation`
  const who = { pane: bind.pane, pid: bind.pid, start: bind.start, session: bind.session }
  // (Its screens kept, an answer refused as only a countdown on it went on
  // sent again: steadyReply.ts.)
  const screen = async () => keepShown(await api<Screen>('POST', base + '/screen', who))
  return {
    key: [sessionId, bind.pane, bind.pid, bind.start, bind.session].join(' '),
    agent: bind.agent,
    screen,
    // (Claude Code's alone count down: another's answers are not sent again — review587.)
    reply: ({ alive, once, ...b }) => steadily((x) => api('POST', base + '/reply', { ...who, ...x }), screen, b, once || bind.agent !== 'claude' ? () => false : alive),
    // (Claude Code's, in tmux: as tried — plans/5-claude-own-words.md. Its
    // tries made by ownWords.)
    fill: canFill && bind.agent === 'claude' ? (b) => api('POST', base + '/reply', { ...who, ...b, fill: true }) : undefined,
    live: (bind.agent ?? 'claude') === 'claude' ? { sid: sessionId, press: () => who } : undefined,
  }
}

// What answering an agent takes: its screen as it is now, the choices on
// it, and sending keys or a line of text, which goes to the pane only if
// the screen is still the one shown.
// onLoaded: the screen has been shown (the request to answer has been seen).
// resizedAt: when the terminal's size last changed; herdr then redraws the
// pane, so the screen is read again a moment later.
// onSent: after an answer went through.
// quiet: no toast once sent (the card in the thread says so in its place).
export function useReply(target: ReplyTarget, { onLoaded, resizedAt, onSent, quiet }: { onLoaded?: () => void; resizedAt?: number; onSent: () => void; quiet?: boolean }) {
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
      await targetRef.current.reply({ digest: screen.digest, alive: () => live.current, ...body })
      // The text sent goes from what is kept for the pane (not a newer one
      // typed while this answer was on its way).
      if ('text' in body && getReplyDraft(draftKey).trim() === body.text) {
        keepReplyDraft(draftKey, '')
        // (Shown too, not only kept: the next question here starts empty.)
        if (live.current) setTextState((v) => (v.trim() === body.text ? '' : v))
      }
      if (!quiet) toast(t('reply.sent'))
      if (live.current) onSent()
    } catch (e) {
      if (!live.current) return
      if (e instanceof ApiError && (e.code === 'screen_changed' || e.code === 'already_answered')) {
        // (herdr's answer says the screen as it is now; the conversation's is read anew.)
        if (typeof e.extra.digest === 'string') setScreen(keepShown({ text: String(e.extra.text ?? ''), digest: e.extra.digest, cut: e.extra.cut === true }))
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
  const opts = screen ? choices(screen.text, target.agent) : []
  return { screen, setScreen, failed, reading, changed, busy, error, text, setText, load, send, submit, opts }
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
          {opts.length > 0 && block && <Asked lines={block.lines} head={block.head} />}
          {(opts.length === 0 || screenOpen || (!block && !questionOnly)) && (
            <div className="reply-screen-wrap">
              <pre className="reply-screen" ref={pre} tabIndex={0} aria-label={t('reply.screen')} aria-busy={reading}>
                {screen ? shown ? <ScreenText lines={shown.split('\n')} /> : t('reply.blank') : t('common.loading')}
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

// What a permission prompt asks to be allowed (its lines, as promptBlock
// gives them), read as it is drawn: what it says it is for, above the first
// dashed rule; the command or the edit itself between the rules — the bar
// Claude Code draws along a command left out, its lines as drawn but wrapped
// here, not cut at the card's edge; and what it notes under them (a danger
// it sees, the countdown before it refuses by itself), as words. An edit's
// lines marked added and removed as the screen marks them ("12 +", "12 -",
// and the lines going on from them). Rules other than two: all of it as
// the command is (11-04).
const GUTTER = /^(\s*)│ ?/
// (Its countdown, the sentence whole: "… will automatically block (deny) this
// request in 1:09 (45s, 1m 5s), to avoid …" — steadyReply's WARNING.)
const COUNTDOWN = /^⚠?\s*Claude Code will automatically (?:deny|block) this request in (\d+(?::\d\d)+|\d+\s*[hms](?:\s*\d+\s*[hms])*), to avoid (?:blocking progress on|holding up) an unattended session\.?(.*)$/
const secsOf = (t: string) => {
  let n = 0
  if (t.includes(':')) for (const p of t.split(':')) n = n * 60 + Number(p)
  else for (const [, v, u] of t.matchAll(/(\d+)\s*([hms])/g)) n += Number(v) * (u === 'h' ? 3600 : u === 'm' ? 60 : 1)
  return n
}
function Asked({ lines: all, head = '' }: { lines: string[]; head?: string }) {
  const rules = all.flatMap((l, i) => (dashedRule.test(l) ? [i] : []))
  const two = rules.length >= 2
  const desc = two ? all.slice(0, rules[0]).map((l) => l.trim()).filter(Boolean) : []
  let main = (two ? all.slice(rules[0] + 1, rules[rules.length - 1]) : all).filter((l) => !dashedRule.test(l))
  // (The bar along a command's lines: only a Bash command's, and only where
  // every line of it has one — anything else as it is — review.)
  if (/^Bash command$/i.test(head) && main.some((l) => l.trim()) && main.every((l) => !l.trim() || GUTTER.test(l))) main = main.map((l) => l.replace(GUTTER, '$1'))
  // (Its notes: one begins after a blank line, at a warning sign, or where
  // the bar along its lines begins or ends; the rest go on from the line
  // before, as they were wrapped.)
  const notes: string[] = []
  let gap = true
  let barred = false
  for (const raw of two ? all.slice(rules[rules.length - 1] + 1) : []) {
    const bar = GUTTER.test(raw)
    const l = raw.replace(GUTTER, '').trim()
    if (!l) {
      gap = true
      continue
    }
    if (gap || l.startsWith('⚠') || bar !== barred || !notes.length) notes.push(l)
    else notes[notes.length - 1] += (/[\u3000-\u9fff]$/.test(notes[notes.length - 1]) ? '' : ' ') + l
    gap = false
    barred = bar
  }
  let sign: '' | 'add' | 'del' = ''
  return (
    <>
      {desc.length > 0 && <p className="convo-ask-desc">{desc.join(' ')}</p>}
      <pre className="convo-pre convo-ask-block">
        {main.map((l, i) => {
          const m = /^\s*\d+\s?([-+])/.exec(l)
          if (m) sign = m[1] === '+' ? 'add' : 'del'
          else if (!(sign && /^\s+[-+]/.test(l))) sign = ''
          return (
            <span key={i} className={sign ? 'convo-' + sign : undefined}>
              {l + '\n'}
            </span>
          )
        })}
      </pre>
      {notes.map((n, i) => {
        const c = COUNTDOWN.exec(n)
        return c ? (
          <Fragment key={i + n}>
            <Countdown secs={secsOf(c[1])} />
            {c[2].trim() && <p className="convo-ask-note">{c[2].trim()}</p>}
          </Fragment>
        ) : (
          <p key={i} className="convo-ask-note">{n.replace(/^⚠\s*/, '')}</p>
        )
      })}
    </>
  )
}

// (The time left before Claude Code refuses it by itself, going down here
// from what the screen said when read: read again, it starts again from that.)
function Countdown({ secs }: { secs: number }) {
  const [until] = useState(() => Date.now() + secs * 1000)
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])
  const left = Math.max(0, Math.round((until - now) / 1000))
  return (
    <p className="convo-ask-note is-countdown" role="timer">
      {left > 0 ? t('convo.askAutoNo', { t: Math.floor(left / 60) + ':' + String(left % 60).padStart(2, '0') }) : t('convo.askAutoNoNow')}
    </p>
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

// What a permission asks, from the log, where the screen shows its end: the
// last call's command (Bash) or file (an edit), if the last of its words —
// spaces and the box's own lines aside, as the screen wraps them — are on the
// screen. null: not that call, or none.
const SPACES = /[\s│|]+/g
function fromLog(call: ConvoTool | undefined, screen: string): { kind: 'run' | 'edit'; what: string; desc?: string } | null {
  if (!call?.detail) return null
  const run = call.name === 'Bash'
  const edit = /^(?:Edit|MultiEdit|Write|NotebookEdit)$/.test(call.name)
  if (!run && !edit) return null
  const tail = call.detail.replace(SPACES, '').slice(-24)
  if (tail.length < 4 || !screen.replace(SPACES, '').includes(tail)) return null
  return { kind: run ? 'run' : 'edit', what: call.detail, desc: run ? call.title : undefined }
}

// The same, in the conversation, over the composer: the question, what it is
// of (from the log), its choices (keys 1–9, Esc), a line of text, and the
// screen itself on demand.
// call: what the log says is asked (the last call of the agent's, not
// come back): a question of Claude Code's or a plan, drawn from it where
// the screen shows that very one (see askCard).
// onAnswered: told when it has been answered here (and when that no longer
// holds: another question came), so the page says so before the agent's
// state, read every few seconds, does.
// pending: the agent's last call not come back, whatever it is — what a
// permission asks of, drawn from the log where the screen does not show its
// start (fromLog).
// A question's card fades in once, as it comes (motion-1105): the one thing
// on the page that waits for the person. Once a question (its agent, and
// the call asking where the log says) — not again as its screen is read
// anew, its card is drawn another way, or it is gone back to.
const asksSeen = new Set<string>()
// (Told by its call, from the log or read off the screen. One not told yet —
// its card just come, the screen not read — is new: it fades in, and is not
// remembered, or every later untold one would be taken for it.)
const told = (id: string) => !id.endsWith('|')
function useFreshAsk(id: string): boolean {
  const [at, setAt] = useState(() => ({ id, fresh: !told(id) || !asksSeen.has(id) }))
  // (Another question in the same card: it fades in as it comes. Not the
  // same one told apart only now, its call found after the card came: it
  // faded in already.)
  if (at.id !== id) setAt({ id, fresh: told(at.id) ? !asksSeen.has(id) : at.fresh }) // (told mid-fade: the fade runs on)
  useEffect(() => {
    if (told(id)) asksSeen.add(id)
    if (!at.fresh) return
    const timer = setTimeout(() => setAt((a) => (a.id === id ? { id, fresh: false } : a)), 400) // (past its fade: a card drawn anew does not fade again)
    return () => clearTimeout(timer)
  }, [id, at.fresh])
  return at.id === id && at.fresh
}

export function InlineReply({ target, onLoaded, resizedAt, active = true, held, call, pending, onAnswered, waitSince }: { target: ReplyTarget; onLoaded?: () => void; resizedAt?: number; active?: boolean; held?: () => boolean; call?: ConvoTool; pending?: ConvoTool; onAnswered?: (yes: boolean) => void; waitSince?: number }) {
  const toast = useToast()
  const showTerminal = useContext(ShowTerminal)
  const [sent, setSent] = useState<string | null>(null) // (the screen answered)
  const answeredRef = useRef(onAnswered)
  answeredRef.current = onAnswered
  useEffect(() => answeredRef.current?.(sent !== null), [sent])
  useEffect(() => () => answeredRef.current?.(false), [])
  const [shown, setShown] = useState(false)
  const answered = useRef('')
  const { screen, setScreen, failed, reading, changed, busy: sending, error: sendError, text, setText, load, send, submit, opts } = useReply(target, { onLoaded, resizedAt, onSent: () => setSent(answered.current), quiet: true })
  // A permission's amend: the person's words with Yes or No (Tab on it, then
  // the words, then Enter: ownWords). Typed, or Enter perhaps gone, and
  // failed: said so, not offered again on this prompt.
  // (The card still shown: one closed or gone to another agent goes no further.)
  const shownNow = useRef(true)
  useEffect(() => {
    shownNow.current = true
    return () => {
      shownNow.current = false
    }
  }, [])
  // (The person's own words go on only while this card is theirs: shown, in front, its page visible — review513/515.)
  const stillHere = () => shownNow.current && activeRef.current && document.visibilityState === 'visible'
  const [amending, setAmending] = useState(false)
  const [amendError, setAmendError] = useState('')
  const [amendStuck, setAmendStuck] = useState('')
  const amend = (n: number, yes: boolean, words: string) => {
    if (!screen || amending) return
    setAmending(true)
    setAmendError('')
    const on = screen.digest
    ownWords(target, screen, permissionAmend(n, yes), words, stillHere)
      .then(() => {
        answered.current = on
        setSent(on)
      })
      .catch((e) => {
        if (e instanceof NotAsShown) setAmendError(t('reply.changed'))
        else {
          setAmendStuck(on)
          setAmendError(t(e instanceof Filled ? 'own.filled' : e instanceof MaybeSent ? 'own.maybeSent' : 'own.filled'))
        }
        void load()
      })
      .finally(() => setAmending(false))
  }
  // (Its log without a question of Claude Code's not yet answered — in auto
  // mode it writes it only once answered — the one its screen shows, drawn
  // as such: where Claude Code waits, drawn as its question is, with the
  // line for one's own words last. A question the log has, other than the
  // screen's: neither. See askCard's screenCall.)
  const read = screen ? readAskScreen(screen.text) : ({ kind: 'unknown' } as const)
  const logAsks = !!call && !call.done && call.name === 'AskUserQuestion'
  const fromScreen = !logAsks && target.agent === 'claude' && read.kind === 'question' ? screenCall(read) : undefined
  // (A review page so drawn: its own id, never empty — an empty one never matches what a key not known to have done set. review529-2)
  const reviewId = !logAsks && target.agent === 'claude' && reviewAsDrawn(read) && read.kind === 'review' ? 'screen-review:' + JSON.stringify(read.answers) : undefined
  // (What a key not known to have done stops: this question, as the log or
  // else the screen tells it — another one, the card again. review528-5)
  // (Told first by when it began to wait — one wait, one question, known as the card comes,
  // gone back to or not; else by its call, from the log or read off the screen — review2.)
  const fresh = useFreshAsk(target.key + '|' + (waitSince ? '@' + waitSince : (call?.id ?? pending?.id ?? fromScreen?.id ?? '')))
  const driven = useDrive(screen, setScreen, load, () => setSent(answered.current), logAsks ? (call!.id ?? '') : (fromScreen?.id ?? reviewId ?? (call && !call.done ? (call.id ?? '') : '')))
  const busy = sending || driven.busy || amending
  const error = sendError || driven.error || amendError
  const state = !driven.unsure ? read : ({ kind: 'unknown' } as const)
  const onScreen = state.kind === 'question' ? fromScreen : undefined
  const asking = onScreen ?? call
  const askAt = onScreen ? 0 : askOn(call, state)
  const lead = onScreen && screen ? askLead(screen.text) : undefined
  // (Claude Code's question drawn from its screen, its log not having it: on
  // the host, the hook that has it written at once may be offered — askHook.tsx.)
  const hookHint =
    target.host && asksFromScreen({ agent: target.agent, logAsks, screen: screen?.text, drawn: !!fromScreen || !!reviewId }) ? <AskHookHint hostId={target.host.id} hostName={target.host.name} /> : null
  const structured =
    !!screen &&
    (askAt >= 0 ||
      (state.kind === 'review' && target.agent === 'claude' && (logAsks || ((!call || call.done) && reviewAsDrawn(state)))) ||
      planOn(call, state, screen.text))
  // (Not known what a key did: the screen, for the person to look at.)
  useEffect(() => {
    if (driven.unsure) setShown(true)
  }, [driven.unsure])
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
  // (Codex's: its approval's words, or its question, as its screen draws them.)
  const cx = screen && target.agent === 'codex' ? codexAsk(screen.text) : null
  const waiting = t('convo.waiting', { agent: agentName(target.agent) })
  // (What is asked not told apart on the screen, the screen itself shows; a
  // question of Claude Code's is all its choices.)
  // (Not told apart there — a long command, the box's top gone up off the
  // screen — what the log says it asks: its command or file, where the
  // screen does show that very one (its end found there). 11-05: a long
  // heredoc showed as a stretch of code from its middle, no telling what.)
  const logged = !!screen && !block && !cx && !folds(screen) && opts.length > 0 ? fromLog(pending, screen.text) : null
  const needScreen = !!screen && !block && !cx && !folds(screen) && !logged
  // (Not told apart, the whole screen, before the choices; else behind its button.)
  const screenLines = screen ? screen.text.split('\n') : []
  const linesShown = needScreen ? screenLines : screenLines.slice(-shownLines)
  const screenBox = (
    // (What is asked could not be told apart on the screen: the screen itself;
    // read again from a button over it that says so, not one over its words.)
    <div className="reply-screen-wrap convo-ask-screen">
      <div className="convo-ask-screen-head">
        <button type="button" className={'btn btn-ghost btn-sm convo-ask-act reply-refresh-btn' + (reading ? ' reading' : '')} onClick={() => void load()} disabled={busy || reading}>
          <RotateCw size={14} aria-hidden="true" />
          {t('reply.refresh')}
        </button>
      </div>
      <pre className="reply-screen" ref={pre} tabIndex={0} aria-label={t('reply.screen')} aria-busy={reading}>
        {screen ? linesShown.join('').length ? <ScreenText lines={linesShown} /> : t('reply.blank') : t('common.loading')}
      </pre>
    </div>
  )
  useLayoutEffect(() => {
    // (Its end: what it asks is drawn last, at the bottom of the screen.)
    if (pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [screen, shown, !block])
  // (Shown as the terminal: read now and then here too, the card coming back
  // where the screen, half drawn when first read, turns out to be one it knows.)
  const liveNow = needScreen && !!target.live && !structured && !failed && sent === null
  const loadNow = useRef(load)
  loadNow.current = load
  useEffect(() => {
    if (!liveNow || !active) return
    const timer = setInterval(() => {
      if (!document.hidden) void loadNow.current()
    }, 3000)
    return () => clearInterval(timer)
  }, [liveNow, active])
  // Keys answer only the screen the card was taken in hand on (touched,
  // or its focus come in): another screen, read meanwhile, is looked at
  // again (touched again) before a key answers it.
  const [armedFor, setArmedFor] = useState('')
  const arm = () => setArmedFor(screen?.digest ?? '')
  const armed = !!screen && armedFor === screen.digest
  const confirm = useConfirm(screen, opts, (keys) => void send({ keys }))
  // (The card drawn from the log answers by its own steps: no keys here.)
  const ready = sent === null && !!screen && !busy && !failed && armedFor === screen.digest && !structured
  const cardRef = useRef<HTMLDivElement>(null)
  useAnswerKeys(active && !structured, ready, [...opts.map((o) => o.key), 'esc'], held, cardRef, (k) => (k === 'esc' ? void send({ keys: [k] }) : confirm.choose(k)))
  if (sent !== null) return <p className="convo-ask-sent" role="status">{t('convo.answeredHere')}</p>
  // (What it asks not made out: the terminal itself, with its colours and its
  // layout, keys going to it. Not the words of a screen and two buttons: that
  // was a third way of showing it, worse than either.)
  if (needScreen && target.live && !failed && !structured) {
    return (
      <div className="convo-ask-live">
        {hookHint}
        <PanelWindow key={target.key} sid={target.live.sid} press={target.live.press} active={active} onSettled={() => {}} toast={toast} live={{ note: t('panel.waiting'), onFull: showTerminal }} />
      </div>
    )
  }
  if (structured && screen && (asking || state.kind === 'review')) {
    return (
      <div className={'convo-ask' + (fresh ? ' is-new' : '')} role="group" aria-label={waiting} aria-busy={busy}>
        {state.kind === 'question' && asking ? (
          <AskPanel key={(asking.id ?? '') + ':' + askAt} call={asking} index={askAt} shown={state} target={target} drive={driven.drive} busy={busy} alive={stillHere} lead={lead} />
        ) : state.kind === 'review' ? (
          <ReviewPanel shown={state} digest={screen.digest} target={target} drive={driven.drive} busy={busy} />
        ) : state.kind === 'plan' && call ? (
          <PlanPanel call={call} shown={state} digest={screen.digest} target={target} drive={driven.drive} busy={busy} onScreen={() => setShown(true)} alive={stillHere} />
        ) : null}
        {changed && (
          <p className="reply-changed" role="alert">
            {t(changed === 'answered' ? 'reply.answered' : 'reply.changed')}
          </p>
        )}
        <div className="convo-ask-more">
          <button type="button" className="btn btn-ghost btn-sm convo-ask-act" aria-expanded={shown} onClick={() => setShown(!shown)}>
            {t(shown ? 'convo.hideScreen' : 'convo.showScreen')}
          </button>
          <button type="button" className="btn btn-ghost btn-sm convo-ask-act" disabled={busy} onClick={() => void send({ keys: ['esc'] })}>
            {t('convo.pressEscCancel')}
          </button>
        </div>
        {shown && screenBox}
        {error && (
          <p className="err" role="alert">
            {error}
          </p>
        )}
        {hookHint}
      </div>
    )
  }
  const q = cx ? cx.question : screen ? question(screen.text) : ''
  const shownQ = (block && /^Bash command$/i.test(block.head)) || logged?.kind === 'run' ? (/^Do you want to proceed\?$/i.test(q) || logged ? t('convo.askRun') : questionText(q)) : logged?.kind === 'edit' ? t('convo.askEdit') : questionText(q)
  // (A question of Codex's: each choice's label and description apart.)
  const shownOpts =
    cx?.kind === 'question'
      ? codexOptions(opts.map((o) => o.label)).map((c, i) => ({ ...opts[i], ...c, ...(/^None of the above$/i.test(c.label) ? { desc: t('convo.askNotes') } : {}) }))
      : opts
  return (
    <div className={'convo-ask' + (fresh ? ' is-new' : '')} role="group" aria-label={waiting} ref={cardRef} onPointerDown={arm} onFocus={arm}>
      <div className="convo-ask-q">{shownQ || waiting}</div>
      {cx?.kind === 'question' && cx.total > 1 && <p className="convo-ask-of">{t('convo.askQuestionOf', { i: cx.index, n: cx.total })}</p>}
      {driven.unsure && <p className="reply-changed" role="alert">{t('ask.unsure')}</p>}
      {block && <Asked lines={block.lines} head={block.head} />}
      {logged && (
        <div className="convo-ask-asked">
          {logged.desc && <p className="convo-ask-desc">{logged.desc}</p>}
          <pre className="convo-ask-block">{logged.what}</pre>
        </div>
      )}
      {cx && cx.kind !== 'question' && cx.lines.length > 0 && <Asked lines={cx.lines} />}
      {(needScreen || !!screen?.cut) && !failed && <p className="convo-ask-unread">{t(screen?.cut ? 'reply.cut' : 'convo.askUnread')}</p>}
      {!failed && hookHint}
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
              {shownOpts.map((o) => {
                const c = choiceText(o.label)
                const lasts = lasting(o.label)
                return (
                  <button key={o.key} type="button" className={'convo-ask-opt' + (confirm.asked === o.key ? ' is-confirm' : '')} disabled={!screen || busy} onClick={() => confirm.choose(o.key)}>
                    <span className="convo-ask-key is-num" aria-hidden="true">{o.key}</span>
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
          {(() => {
            // (Claude Code's permission, amend offered: Yes and No with the person's words.)
            if (!target.fill || !screen || !/Tab to amend/.test(screen.text) || amendStuck) return null
            const yes = opts.find((o) => /^Yes$/.test(o.label))
            const no = opts.find((o) => /^No$/.test(o.label))
            if (!yes && !no) return null
            return (
              <>
                {yes && <OwnBox label={t('own.yesWith')} keep={call?.id ? `${target.key} ${call.id} yes` : undefined} busy={busy} onGo={(w) => amend(Number(yes.key), true, w)} />}
                {no && <OwnBox label={t('own.noWith')} keep={call?.id ? `${target.key} ${call.id} no` : undefined} busy={busy} onGo={(w) => amend(Number(no.key), false, w)} />}
              </>
            )
          })()}
          <form className="convo-ask-text" onSubmit={submit}>
            <input
              className="input"
              value={text}
              maxLength={2000}
              placeholder={t(opts.length > 0 ? 'convo.askOwnText' : 'convo.askText')}
              aria-label={t(opts.length > 0 ? 'convo.askOwnText' : 'convo.askText')}
              autoCapitalize="none"
              autoCorrect="off"
              onChange={(e) => setText(e.target.value)}
            />
            <button className="btn" disabled={!screen || busy || !text.trim()}>
              {t('reply.send')}
            </button>
          </form>
          {/* (The keys, on a line of their own: words, not to be taken for buttons. Only
              once the card is in hand do they answer it: said so till then.) */}
          {!isTouchOnly && opts.length > 0 && (
            <p className="convo-ask-keys">
              {t(
                target.agent === 'codex' ? (armed ? 'convo.askKeysCodex' : 'convo.askKeysArmCodex') : armed ? 'convo.askKeys' : 'convo.askKeysArm',
                { n: opts.length },
              )}
            </p>
          )}
          {(!needScreen || opts.length === 0) && (
            <div className="convo-ask-more">
              {/* (The screen shown whole already: no button to show it.) */}
              {!needScreen && (
                <button type="button" className="btn btn-ghost btn-sm convo-ask-act" aria-expanded={shown} onClick={() => setShown(!shown)}>
                  {t(shown ? 'convo.hideScreen' : 'convo.showScreen')}
                </button>
              )}
              {opts.length === 0 && (
                <button type="button" className={'btn btn-ghost btn-sm convo-ask-act' + (confirm.asked === 'enter' ? ' is-confirm' : '')} disabled={!screen || busy} onClick={confirm.enter}>
                  {t(confirm.asked === 'enter' ? 'reply.enterAgain' : 'convo.pressEnter')}
                </button>
              )}
              {opts.length === 0 && (
                // (With choices, the last one says no, and Esc does too.)
                <button type="button" className="btn btn-ghost btn-sm convo-ask-act" disabled={!screen || busy} onClick={() => void send({ keys: ['esc'] })}>
                  {t('convo.pressEsc')}
                </button>
              )}
            </div>
          )}
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
