import type { ReplyTarget } from './ReplyDialog'

type Screen = { text: string; digest: string; cut?: boolean }
import { codexAsk, codexCursor } from './menu'
import { ApiError } from '../api'

// A Codex question (request_user_input) answered with the person's own
// words: its cursor taken to a choice by the arrows (each step read back
// from its screen), then the words, then Enter. Codex takes words typed
// there as that choice's notes ("answer: None of the above, note: …"):
// a choice's number would choose it at once, with none.
//
// NotAsShown: nothing pressed yet, the screen other than shown. Unsure: a
// key went, what it did not seen (the cursor may have moved; nothing is
// answered without the words and Enter). MaybeAnswered: the words and
// Enter sent, no answer back: it may have been answered.

export class NotAsShown extends Error {}
export class Unsure extends Error {}
export class MaybeAnswered extends Error {}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

// (The same question: its words, and which of how many.)
const sameQ = (a: ReturnType<typeof codexAsk>, b: ReturnType<typeof codexAsk>) =>
  !!a && !!b && a.kind === 'question' && b.kind === 'question' && a.question === b.question && a.index === b.index && a.total === b.total

// (Refused before anything was pressed: the gateway said so.)
const refused = (e: unknown) => e instanceof ApiError && [400, 403, 404, 409, 429].includes(e.status)

export async function codexAnswerWith(target: ReplyTarget, from: Screen, n: number, words: string, alive: () => boolean): Promise<void> {
  const q0 = codexAsk(from.text)
  if (!q0 || q0.kind !== 'question') throw new NotAsShown()
  let s = from
  let pressed = false
  for (let guard = 0; guard < 4; guard++) {
    const cur = codexCursor(s.text)
    if (cur === undefined) throw pressed ? new Unsure() : new NotAsShown()
    if (cur === n) break
    if (!alive()) throw pressed ? new Unsure() : new NotAsShown()
    const d = n - cur
    const keys: string[] = Array(Math.min(4, Math.abs(d))).fill(d > 0 ? 'down' : 'up')
    const want = cur + (d > 0 ? keys.length : -keys.length)
    try {
      await target.reply({ digest: s.digest, keys, once: true })
    } catch (e) {
      if (refused(e)) throw pressed ? new Unsure() : new NotAsShown()
      throw new Unsure()
    }
    pressed = true
    // (Read back until the cursor is seen where it was taken: some three seconds.)
    let got: Screen | null = null
    const until = Date.now() + 3500
    for (let i = 0; Date.now() < until; i++) {
      await sleep(i === 0 ? 150 : 250)
      let x: Screen
      try {
        x = await target.screen()
      } catch {
        continue
      }
      if (sameQ(codexAsk(x.text), q0) && codexCursor(x.text) === want) {
        got = x
        break
      }
    }
    if (!got) throw new Unsure()
    s = got
  }
  if (codexCursor(s.text) !== n || !sameQ(codexAsk(s.text), q0)) throw pressed ? new Unsure() : new NotAsShown()
  if (!alive()) throw pressed ? new Unsure() : new NotAsShown()
  try {
    await target.reply({ digest: s.digest, text: words, once: true })
  } catch (e) {
    if (refused(e)) throw pressed ? new Unsure() : new NotAsShown()
    throw new MaybeAnswered()
  }
}
