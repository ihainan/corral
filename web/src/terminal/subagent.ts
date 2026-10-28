import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { ConvoEntry, ConvoRunning } from './conversation'

// A subagent's own conversation (its log beside its session's), read as the
// conversation is: a part at a time, what changed since. Read when shown;
// again every few seconds while it goes on (known so, or, of one sent off
// by a subagent, while its log keeps growing), each read after the last
// came back; not while the page is hidden or the view is in the back.

interface Answer {
  entries: ConvoEntry[]
  gen: number
  rev: number
  reset?: boolean
  older?: boolean
  older_cut?: boolean
  more?: boolean
  running?: ConvoRunning[]
}

export interface SubConvo {
  entries: ConvoEntry[]
  running: ConvoRunning[]
  older: boolean
  olderCut: boolean
  loading: boolean
  failed: boolean
  growing: boolean // (its log grew lately: what the page takes for going on, where it is told nothing else)
  read: () => void
  readOlder: () => void
}

const EVERY = 3000
const GROWING = 30000 // (unknown whether it goes on: read on while it grew this lately)
const BATCH = 20 // (reads in a row while there is more; then the next round)

export function useSubConvo(sessionId: string, pane: string, agent: string, live: boolean, active: boolean, nested: boolean): SubConvo {
  const [shown, setShown] = useState({ entries: [] as ConvoEntry[], running: [] as ConvoRunning[], older: false, olderCut: false, loading: true, failed: false, grewAt: 0 })
  // (ctl: the read under way, if any; one called off is none.)
  const at = useRef({ gen: 0, rev: 0, byseq: new Map<number, ConvoEntry>(), again: false, grewAt: 0, ctl: null as AbortController | null })
  const liveNow = useRef(live)
  liveNow.current = live
  const activeNow = useRef(active)
  activeNow.current = active
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const goesOn = () => liveNow.current || (nested && Date.now() - at.current.grewAt < GROWING)
  const run = useCallback(
    async (older: boolean) => {
      const a = at.current
      // (Hidden, or the view in the back: read once it is shown again.)
      if (document.hidden || !activeNow.current) return
      if (a.ctl && !a.ctl.signal.aborted) {
        a.again = true // (asked meanwhile: once more, after)
        return
      }
      a.again = false
      const ctl = new AbortController()
      a.ctl = ctl
      setShown((s) => ({ ...s, loading: true }))
      let more = false
      try {
        for (let n = 0; n < BATCH; n++) {
          const q = new URLSearchParams({ pane, agent, gen: String(a.gen), rev: String(a.rev), older: older && n === 0 ? '1' : '0' })
          const r = await api<Answer>('GET', `/api/sessions/${sessionId}/conversation/subagent?` + q.toString(), undefined, { signal: ctl.signal })
          if (ctl.signal.aborted) return
          if (r.reset || r.gen !== a.gen) a.byseq = new Map()
          if (r.entries.length) a.grewAt = Date.now()
          for (const e of r.entries) a.byseq.set(e.seq, e)
          a.gen = r.gen
          a.rev = r.rev
          more = !!r.more
          setShown({ entries: [...a.byseq.values()].sort((x, y) => x.seq - y.seq), running: r.running ?? [], older: !!r.older, olderCut: !!r.older_cut, loading: false, failed: false, grewAt: a.grewAt })
          if (!more || document.hidden || !activeNow.current) break
        }
      } catch {
        if (!ctl.signal.aborted) setShown((s) => ({ ...s, loading: false, failed: true }))
        more = false
      } finally {
        if (a.ctl === ctl) a.ctl = null
        if (!ctl.signal.aborted) {
          clearTimeout(timer.current)
          // (Asked meanwhile, or more to read: at once; going on: in a while.)
          const wait = a.again || more ? 0 : goesOn() ? EVERY : -1
          if (wait >= 0) timer.current = setTimeout(() => !document.hidden && activeNow.current && void run(false), wait)
        }
      }
    },
    [sessionId, pane, agent],
  )
  useEffect(() => {
    const a = at.current
    void run(false)
    // (Shown again: read at once.)
    const onSeen = () => !document.hidden && activeNow.current && void run(false)
    document.addEventListener('visibilitychange', onSeen)
    return () => {
      a.ctl?.abort()
      clearTimeout(timer.current)
      document.removeEventListener('visibilitychange', onSeen)
    }
  }, [run])
  // (Going on now, or just over: read, so that its end is shown too; the
  // view in front again: read.)
  const wasLive = useRef(live)
  useEffect(() => {
    if (live !== wasLive.current) void run(false)
    wasLive.current = live
  }, [live, run])
  const wasActive = useRef(active)
  useEffect(() => {
    if (active && !wasActive.current) void run(false)
    wasActive.current = active
  }, [active, run])
  return { ...shown, growing: Date.now() - shown.grewAt < GROWING, read: () => void run(false), readOlder: () => void run(true) }
}
