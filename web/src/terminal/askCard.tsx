import { useState } from 'react'
import { ApiError } from '../api'
import { t } from '../i18n'
import { Markdown } from './markdown'
import { readAskScreen, sameLoose, sameWords, type AskScreen, type ScreenOption } from './askScreen'
import { choiceText, lasting } from './askText'
import type { ConvoAsk, ConvoTool } from './conversation'
import type { ReplyTarget } from './ReplyDialog'
import { Filled, MaybeSent, NotAsShown, ownOK, ownWords, planChange, questionOwn } from './ownWords'

// The card of a question of Claude Code's (AskUserQuestion) or a plan it
// puts to the person, drawn from what its log says it asked, where the
// screen shows that very question: answered there one question at a time,
// key by key, each key sent only on the screen read just before it (the
// gateway checks that, on the host), each read again after until it shows
// what the key was to do. A key whose fate is not known stops it all.

type Screen = { text: string; digest: string; cut?: boolean }
type Q = Extract<AskScreen, { kind: 'question' }>

// Which of the call's questions the screen shows: the same words, the
// same choices in the same order, ticks or not as asked; else none (the
// card that reads the screen is shown instead).
export function askOn(call: ConvoTool | undefined, s: AskScreen): number {
  if (!call || call.name !== 'AskUserQuestion' || call.done || !call.asks?.length || s.kind !== 'question') return -1
  const asks = call.asks
  if (asks.some((a) => a.cut)) return -1
  if (s.tabs.length > 0 && (s.tabs.length !== asks.length || s.tabs.some((tab, i) => !!asks[i].header && !sameLabel(tab.label, asks[i].header!, [])))) return -1
  if (s.tabs.length === 0 && asks.length !== 1) return -1
  // (Spaces not counted, as the screen may break a word: only where no two
  // of what it could be are alike so — else the words as they are.)
  const loose = (all: string[]) => new Set(all.map((x) => x.replace(/\s+/g, ''))).size === all.length
  const sameQ = loose(asks.map((a) => a.question)) ? sameLoose : sameWords
  const at = asks.findIndex((a: ConvoAsk) => {
    if (!sameQ(a.question, s.question) || !!a.multi !== s.multi) return false
    const shown = s.options.filter((o) => o.n !== s.own)
    const labels = a.options.map((o) => o.label)
    return shown.length === a.options.length && shown.every((o, i) => o.n === i + 1 && sameLabel(o.label, labels[i], labels, o.cut))
  })
  // (Two questions alike: which one this is cannot be told.)
  return at >= 0 && asks.filter((a) => sameQ(a.question, s.question)).length === 1 ? at : -1
}

// (The screen may cut a long label short: then only where the start it
// shows is of that label alone.)
const flat = (x: string) => x.replace(/\s+/g, ' ').trim()
// (cut: its words ran to its column's edge, the rest wrapped under it: then
// it is known by its start, where that is of that label alone.)
function sameLabel(shown: string, asked: string, all: string[], cut = false): boolean {
  if (cut) {
    // (Only its start is known, an ellipsis the screen put at its end not
    // of it: that start must be of this label alone, even where it is all
    // of one.)
    const start = shown.replace(/\s*…$/, '').replace(/\s+/g, '')
    const of = (l: string) => l.replace(/\s+/g, '').startsWith(start)
    return !!start && of(asked) && all.filter(of).length === 1
  }
  if (sameWords(shown, asked)) return true
  // (Spaces not counted only where no other label is the same so.)
  if (sameLoose(shown, asked)) return all.filter((l) => sameLoose(l, asked)).length === 1
  if (!shown.endsWith('…')) return false
  const start = flat(shown.slice(0, -1))
  return !!start && flat(asked).startsWith(start) && all.filter((l) => flat(l).startsWith(start)).length <= 1
}

// The screen shows the same question as q (the same words, choices, ways).
const sameQ = (x: AskScreen, q: Q): x is Q =>
  x.kind === 'question' &&
  sameWords(x.question, q.question) &&
  x.multi === q.multi &&
  x.beside === q.beside &&
  x.tabs.length === q.tabs.length &&
  x.tabs.every((tab, i) => sameWords(tab.label, q.tabs[i].label)) &&
  x.options.length === q.options.length &&
  x.options.every((o, i) => o.n === q.options[i].n && sameWords(o.label, q.options[i].label))

// (The plan's first line, shown on the screen too: that very plan.)
export function planOn(call: ConvoTool | undefined, s: AskScreen, text: string): boolean {
  if (!call || call.name !== 'ExitPlanMode' || call.done || s.kind !== 'plan') return false
  const first = (call.plan ?? '').split('\n').map((l) => flat(l.replace(/^#+\s*/, '').replace(/[`*_]/g, ''))).find(Boolean)
  return !first || flat(text.replace(/[`*_]/g, '')).includes(first)
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

class Unsure extends Error {}

// A refusal that pressed nothing: the gateway said so before any key went
// (the screen or the wait not as shown, the pane busy, the host not
// reached at all); anything else (no answer, cut off, signed out on the
// way back, another error) may have pressed it.
const refused = (e: unknown) =>
  e instanceof ApiError && ([400, 403, 404, 409, 429].includes(e.status) || (e.status === 502 && (e.code === 'host_error' || e.code === 'send_failed')))

const within = <T,>(p: Promise<T>, ms: number) => Promise.race([p, new Promise<never>((_, no) => setTimeout(() => no(new Unsure()), ms))])

// Keys, then the screen read again (now and then, for some three seconds
// in all) until it shows what they were to do; the screen it then shows.
// Not shown by then, or not read: not known what they did.
async function step(target: ReplyTarget, from: Screen, keys: string[], done: (s: AskScreen) => boolean): Promise<Screen> {
  try {
    await target.reply({ digest: from.digest, keys })
  } catch (e) {
    if (refused(e)) throw e
    throw new Unsure()
  }
  const until = Date.now() + 3500
  for (let i = 0; Date.now() < until; i++) {
    await sleep(i === 0 ? 150 : 250)
    let s: Screen
    try {
      s = await within(target.screen(), Math.max(200, until - Date.now()))
    } catch {
      throw new Unsure()
    }
    if (done(readAskScreen(s.text))) return s
  }
  throw new Unsure()
}

type Drive = (run: (now: Screen) => Promise<Screen | null>) => void

// The steps of an answer, one at a time; a refusal (the screen changed,
// it no longer waits) reads it anew; one whose fate is not known leaves
// the card to the screen.
export function useDrive(screen: Screen | null, set: (s: Screen) => void, reload: () => Promise<void>, onDone: () => void, callId: string) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // (For the call it was not known on: a later one is answered here again.)
  const [unsureOn, setUnsureOn] = useState('')
  const unsure = !!unsureOn && unsureOn === callId
  const drive: Drive = (run) => {
    if (!screen || busy || unsure) return
    setBusy(true)
    setError('')
    void Promise.resolve()
      .then(() => run(screen))
      .then((s) => (s && readAskScreen(s.text).kind !== 'unknown' ? set(s) : onDone()))
      .catch((e) => {
        // (The person's own words: refused before anything was typed, read
        // anew; typed, or Enter perhaps gone, said so and not tried again.)
        if (e instanceof NotAsShown) {
          setError(t('reply.changed'))
          void reload()
          return
        }
        if (e instanceof Filled || e instanceof MaybeSent) {
          setUnsureOn(callId || '?')
          setError(t(e instanceof Filled ? 'own.filled' : 'own.maybeSent'))
          void reload()
          return
        }
        if (!refused(e)) {
          setUnsureOn(callId || '?')
          void reload()
        } else {
          setError(e instanceof ApiError && e.code === 'not_waiting' ? t('reply.notWaiting') : t('reply.changed'))
          void reload()
        }
      })
      .finally(() => setBusy(false))
  }
  return { busy, error, unsure, drive }
}

const at = (s: Screen) => readAskScreen(s.text)

// Moves to choice n (by ↑↓, at most four keys a time), then Enter.
// (Each step from a screen that still shows the question answered, as it
// was: another one drawn meanwhile, alike or not, is not answered.)
async function pickWithEnter(target: ReplyTarget, now: Screen, q0: Q, n: number): Promise<Screen> {
  let s = now
  for (let guard = 0; guard < 4; guard++) {
    const q = at(s)
    if (!sameQ(q, q0)) throw new Unsure()
    const cur = q.options.find((o) => o.cursor)?.n
    if (cur === n) break
    if (cur === undefined) throw new Unsure()
    const d = n - cur
    const keys = Array(Math.min(4, Math.abs(d))).fill(d > 0 ? 'down' : 'up')
    const want = cur + (d > 0 ? keys.length : -keys.length)
    s = await step(target, s, keys, (x) => sameQ(x, q0) && x.options.find((o) => o.cursor)?.n === want)
  }
  const q = at(s)
  if (!sameQ(q, q0) || q.options.find((o) => o.cursor)?.n !== n) throw new Unsure()
  return step(target, s, ['enter'], (x) => !sameQ(x, q0))
}

// Ticks as wanted (a digit each, read again after each), then the Submit line, Enter.
async function tickAndGo(target: ReplyTarget, now: Screen, q0: Q, want: Set<number>): Promise<Screen> {
  let s = now
  for (const o of q0.options) {
    const q = at(s)
    if (!sameQ(q, q0)) throw new Unsure()
    const was = q.options.find((p) => p.n === o.n)!
    if (o.n === q0.own || want.has(o.n) === !!was.ticked) continue
    // (That tick turned, the others as they were.)
    s = await step(target, s, [String(o.n)], (x) => sameQ(x, q0) && x.options.every((p, i) => p.n === q0.own || !!p.ticked === (p.n === o.n ? want.has(o.n) : !!q.options[i].ticked)))
  }
  const q = at(s)
  if (!sameQ(q, q0)) throw new Unsure()
  // (Submit: the line after the last choice; the cursor on it already, or on a choice.)
  if (!q.submit?.cursor) {
    const cur = q.options.find((o) => o.cursor)?.n
    if (cur === undefined) throw new Unsure()
    let left = q.options.length - cur + 1
    while (left > 0) {
      const n = Math.min(4, left)
      left -= n
      const last = left === 0
      s = await step(target, s, Array(n).fill('down'), (x) => sameQ(x, q0) && (last ? !!x.submit?.cursor : true))
    }
  }
  return step(target, s, ['enter'], (x) => !sameQ(x, q0))
}

export function AskPanel({ call, index, shown, target, drive, busy, alive }: { call: ConvoTool; index: number; shown: Q; target: ReplyTarget; drive: Drive; busy: boolean; alive: () => boolean }) {
  const ask = call.asks![index]
  const [ticks, setTicks] = useState<Set<number> | null>(null)
  const ticked = ticks ?? new Set(shown.options.filter((o) => o.ticked).map((o) => o.n))
  const [preview, setPreview] = useState(-1)
  // (A lone question with nothing beside: a digit answers it at once.)
  const pick = (n: number) =>
    drive((now) => {
      if (!sameQ(at(now), shown)) throw new Unsure()
      return shown.tabs.length === 0 && !shown.beside ? step(target, now, [String(n)], (x) => !sameQ(x, shown)) : pickWithEnter(target, now, shown, n)
    })
  return (
    <div className="convo-ask-structured">
      {shown.tabs.length > 1 && (
        <ol className="convo-ask-tabs" aria-label={t('ask.questions')}>
          {call.asks!.map((a, i) => (
            <li key={i} className={(i === index ? 'is-now' : '') + (shown.tabs[i]?.answered ? ' is-done' : '')} aria-current={i === index || undefined}>
              {a.header || t('ask.nth', { n: i + 1 })}
              {shown.tabs[i]?.answered && <span className="sr-only">{' · ' + t('ask.answered')}</span>}
            </li>
          ))}
        </ol>
      )}
      <div className="convo-ask-q">{ask.question}</div>
      {ask.multi && <p className="convo-ask-hint">{t('ask.many')}</p>}
      <div className="convo-ask-opts" role="group" aria-label={t('reply.choices')}>
        {ask.options.map((o, i) => {
          const n = i + 1
          return (
            <div key={n} className="convo-ask-opt-wrap">
              <button
                type="button"
                className={'convo-ask-opt' + (ask.multi && ticked.has(n) ? ' is-ticked' : '')}
                disabled={busy}
                aria-pressed={ask.multi ? ticked.has(n) : undefined}
                onClick={() => {
                  if (!ask.multi) return pick(n)
                  const next = new Set(ticked)
                  if (next.has(n)) next.delete(n)
                  else next.add(n)
                  setTicks(next)
                }}
              >
                <span className="convo-ask-key">{ask.multi ? (ticked.has(n) ? '✓' : '') : n}</span>
                <span className="convo-ask-what">
                  <span className="convo-ask-label">{o.label}</span>
                  {o.description && <span className="convo-ask-desc">{o.description}</span>}
                </span>
              </button>
              {o.preview && (
                <button type="button" className="link-btn convo-ask-preview-btn" aria-expanded={preview === n} onClick={() => setPreview(preview === n ? -1 : n)}>
                  {t(preview === n ? 'ask.hidePreview' : 'ask.preview')}
                </button>
              )}
              {o.preview && preview === n && <pre className="convo-pre convo-ask-preview">{o.preview}</pre>}
            </div>
          )
        })}
      </div>
      {ask.multi && (
        <button type="button" className="btn btn-primary convo-ask-go" disabled={busy} onClick={() => drive((now) => tickAndGo(target, now, shown, ticked))}>
          {t('ask.goOn')}
        </button>
      )}
      {shown.own !== undefined &&
        (!ask.multi && target.fill ? (
          <OwnBox label={t('own.answer')} busy={busy} onGo={(text) => drive((now) => ownWords(target, now, questionOwn(shown.own!), text, alive))} />
        ) : (
          <p className="convo-ask-hint">{t('ask.ownInTerminal')}</p>
        ))}
    </div>
  )
}

// A line of the person's own, into a row of the prompt that takes it.
export function OwnBox({ label, busy, onGo, hint }: { label: string; busy: boolean; onGo: (text: string) => void; hint?: string }) {
  const [text, setText] = useState('')
  const ok = ownOK(text)
  const long = [...text].length > 1000
  return (
    <form
      className="convo-ask-text convo-ask-own"
      onSubmit={(e) => {
        e.preventDefault()
        if (ok && !busy) onGo(text)
      }}
    >
      <input className="input" value={text} placeholder={label} aria-label={label} autoCapitalize="none" autoCorrect="off" onChange={(e) => setText(e.target.value.replace(/[\r\n]+/g, ' '))} />
      <button className="btn" disabled={busy || !ok}>
        {t('own.go')}
      </button>
      {long && <p className="convo-ask-hint is-warn">{t('own.tooLong')}</p>}
      {hint && <p className="convo-ask-hint">{hint}</p>}
    </form>
  )
}

// The page that reviews the answers before they go: as the screen says it.
export function ReviewPanel({ shown, digest, target, drive, busy }: { shown: Extract<AskScreen, { kind: 'review' }>; digest: string; target: ReplyTarget; drive: Drive; busy: boolean }) {
  return (
    <div className="convo-ask-structured">
      <div className="convo-ask-q">{t('ask.review')}</div>
      {shown.incomplete && <p className="convo-ask-hint is-warn">{t('ask.incomplete')}</p>}
      <dl className="convo-ask-answers">
        {shown.answers.map((a, i) => (
          <div key={i}>
            <dt>{a.question}</dt>
            <dd>{a.answer}</dd>
          </div>
        ))}
      </dl>
      <Choices digest={digest} options={shown.options} busy={busy} onPick={(n) => drive((now) => step(target, now, [String(n)], (x) => x.kind !== 'review'))} />
    </div>
  )
}

// A plan put to the person: the plan as the log has it, the choices as the
// screen shows them (one that lasts beyond this: a second tap).
export function PlanPanel({ call, shown, digest, target, drive, busy, onScreen, alive }: { call: ConvoTool; shown: Extract<AskScreen, { kind: 'plan' }>; digest: string; target: ReplyTarget; drive: Drive; busy: boolean; onScreen: () => void; alive: () => boolean }) {
  return (
    <div className="convo-ask-structured">
      <div className="convo-ask-q">{t('ask.plan')}</div>
      {call.plan ? (
        <div className="convo-ask-plan convo-md">
          <Markdown text={call.plan} />
        </div>
      ) : (
        <p className="convo-ask-hint">{t('ask.noPlan')}</p>
      )}
      {call.plan_cut && (
        <p className="convo-ask-hint">
          {t('ask.planCut')}{' '}
          <button type="button" className="link-btn" onClick={onScreen}>
            {t('convo.showScreen')}
          </button>
        </p>
      )}
      <Choices digest={digest} options={shown.options.filter((o) => !/^Tell Claude what to change$/i.test(o.label))} busy={busy} onPick={(n) => drive((now) => step(target, now, [String(n)], (x) => x.kind !== 'plan'))} />
      {(() => {
        const change = shown.options.find((o) => /^Tell Claude what to change$/i.test(o.label))
        return change && target.fill ? (
          <OwnBox label={t('own.change')} hint={t('own.changeHint')} busy={busy} onGo={(text) => drive((now) => ownWords(target, now, planChange(change.n), text, alive))} />
        ) : (
          <p className="convo-ask-hint">{t('ask.changeInTerminal')}</p>
        )
      })()}
    </div>
  )
}

// Choices as the screen shows them; one that lasts beyond this asks a second tap.
function Choices({ digest, options, busy, onPick }: { digest: string; options: ScreenOption[]; busy: boolean; onPick: (n: number) => void }) {
  // (Asked again on this screen only: another one read meanwhile asks anew.)
  const [askedOn, setAskedOn] = useState({ n: -1, digest: '' })
  const asked = askedOn.digest === digest ? askedOn.n : -1
  const setAsked = (n: number) => setAskedOn({ n, digest })
  return (
    <div className="convo-ask-opts" role="group" aria-label={t('reply.choices')}>
      {options.map((o) => {
        const c = choiceText(o.label)
        const lasts = lasting(o.label)
        return (
          <button
            key={o.n}
            type="button"
            className={'convo-ask-opt' + (asked === o.n ? ' is-confirm' : '')}
            disabled={busy}
            onClick={() => {
              if (lasts && asked !== o.n) return setAsked(o.n)
              setAsked(-1)
              onPick(o.n)
            }}
          >
            <span className="convo-ask-key">{o.n}</span>
            <span className="convo-ask-what" title={c.original || undefined}>
              <span className="convo-ask-label">{c.text}</span>
              {lasts && <span className="convo-ask-desc is-lasting">{t(asked === o.n ? 'convo.askConfirm' : lasts === 'rule' ? 'convo.askRule' : 'convo.askMode')}</span>}
            </span>
          </button>
        )
      })}
    </div>
  )
}
