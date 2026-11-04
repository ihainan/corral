import { getPrefs, type Prefs } from '../prefs'
import { taken, wanted, type ChimeKind } from './chimeRules'

export { agentWho, BellFilter, ChimeCounter, countsOf, oscNotice } from './chimeRules'

// A sound on this device when an agent finishes, or comes to need the
// person (the "chime" setting), or a program in a terminal rings its bell.
//
// Agents: the gateway counts, for each herdr pane, how many times its agent
// finished and came to need the person, as its "finished" alert judges it
// (subagents it sent off back; internal/web/herdr_chime.go). A count gone
// up is a sound, however short the turn was between two readings. The
// first count a page sees of a pane (and one gone down: the gateway began
// counting again) is where it stands, not a sound.
//
// Only a page in sight sounds (one hidden may not be let to; the alert on
// the device is for that). Pages of one browser take each sound once
// between them (who takes it first; a key kept a few seconds).
//
// The sound is made here (no file is fetched). A browser lets a page play
// sound only once the person has done something on it: the first tap or
// key does that (unlockChime).

let ctx: AudioContext | null = null
let unlocked = false

function audio(): AudioContext | null {
  if (ctx) return ctx
  const C = window.AudioContext ?? (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext
  if (!C) return null
  try {
    ctx = new C()
  } catch {
    return null
  }
  return ctx
}

// unlockChime: from now on (the first tap or key on the page), sound may
// be played. Set up once.
export function unlockChime() {
  if (unlocked) return
  unlocked = true
  const on = () => {
    const a = audio()
    if (a && a.state === 'suspended') void a.resume().catch(() => {})
    // (iOS: a sound started in the gesture itself, silent, opens the way.)
    if (a) {
      try {
        const g = a.createGain()
        g.gain.value = 0
        const o = a.createOscillator()
        o.connect(g).connect(a.destination)
        o.start()
        o.stop(a.currentTime + 0.01)
      } catch {
        /* no matter */
      }
    }
    window.removeEventListener('pointerdown', on, true)
    window.removeEventListener('keydown', on, true)
  }
  window.addEventListener('pointerdown', on, true)
  window.addEventListener('keydown', on, true)
}

// The tones: notes (frequency, start, length in seconds) at a soft level.
const TONES: Record<Prefs['chimeTone'], { f: number; at: number; len: number; type: OscillatorType }[]> = {
  bell: [
    { f: 880, at: 0, len: 0.5, type: 'sine' },
    { f: 1320, at: 0.12, len: 0.6, type: 'sine' },
  ],
  soft: [
    { f: 523.25, at: 0, len: 0.35, type: 'triangle' },
    { f: 659.25, at: 0.16, len: 0.45, type: 'triangle' },
  ],
  ping: [{ f: 1568, at: 0, len: 0.25, type: 'sine' }],
}

// playChime plays a tone now (the settings' "try it" too: a tap, so it is
// let to). needs: a little higher, told apart from "finished".
export function playChime(tone: Prefs['chimeTone'], needs = false) {
  const a = audio()
  if (!a) return false
  if (a.state === 'suspended') void a.resume().catch(() => {})
  const t0 = a.currentTime + 0.02
  for (const n of TONES[tone] ?? TONES.bell) {
    try {
      const o = a.createOscillator()
      const g = a.createGain()
      o.type = n.type
      o.frequency.value = n.f * (needs ? 1.25 : 1)
      g.gain.setValueAtTime(0.0001, t0 + n.at)
      g.gain.exponentialRampToValueAtTime(0.18, t0 + n.at + 0.02)
      g.gain.exponentialRampToValueAtTime(0.0001, t0 + n.at + n.len)
      o.connect(g).connect(a.destination)
      o.start(t0 + n.at)
      o.stop(t0 + n.at + n.len + 0.05)
    } catch {
      return false
    }
  }
  return true
}

// chime: a sound of kind for key (what it is of, the same in every page
// that sees it), if asked for, the page in sight, and not taken already.
export function chime(kind: ChimeKind, key: string) {
  const p = getPrefs()
  if (!wanted(kind, p) || document.visibilityState !== 'visible') return
  // (Only a page let to play takes it: one not yet tapped would take it
  // from one that can — review585-8.)
  if (!ctx || ctx.state !== 'running') return
  if (taken(kind + ' ' + key)) return
  playChime(p.chimeTone, kind === 'needs')
}
