import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { ConvoEntry, ConvoRunning } from './conversation'
import { createSubConvoPoller, type SubPollPage } from './sideState'

// A subagent's own conversation (its log beside its session's), read as the
// conversation is: a part at a time, what changed since. Read when shown;
// again every few seconds while it goes on (known so, or, of one sent off
// by a subagent, while its log keeps growing), each read after the last
// came back; not while the page is hidden or the view is in the back.

type Answer = SubPollPage<ConvoEntry, ConvoRunning>

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

export function useSubConvo(sessionId: string, pane: string, agent: string, live: boolean, active: boolean, nested: boolean, cli?: string, cliParent?: string, cliChain?: string[]): SubConvo {
  const [shown, setShown] = useState({ entries: [] as ConvoEntry[], running: [] as ConvoRunning[], older: false, olderCut: false, loading: true, failed: false, grewAt: 0 })
  const liveNow = useRef(live)
  liveNow.current = live
  const activeNow = useRef(active)
  activeNow.current = active
  const pollerRef = useRef<{ run: (older: boolean) => Promise<void>; stop: () => void } | null>(null)
  const cliChainKey = cliChain?.length ? cliChain.join(',') : ''

  useEffect(() => {
    const poller = createSubConvoPoller<ConvoEntry, ConvoRunning>({
      sessionId,
      pane,
      agent,
      cli,
      cliParent,
      cliChain: cliChainKey ? cliChainKey.split(',') : undefined,
      nested,
      live: () => liveNow.current,
      active: () => activeNow.current,
      hidden: () => document.hidden,
      everyMs: EVERY,
      growingMs: GROWING,
      batch: BATCH,
      fetchPage: (url, signal) => api<Answer>('GET', url, undefined, { signal }),
      onState: (update) => setShown(update),
    })
    pollerRef.current = poller
    void poller.run(false)
    // (Shown again: read at once.)
    const onSeen = () => !document.hidden && activeNow.current && void poller.run(false)
    document.addEventListener('visibilitychange', onSeen)
    return () => {
      poller.stop()
      if (pollerRef.current === poller) pollerRef.current = null
      document.removeEventListener('visibilitychange', onSeen)
    }
  }, [sessionId, pane, agent, nested, cli, cliParent, cliChainKey])

  const read = useCallback(() => {
    void pollerRef.current?.run(false)
  }, [])
  const readOlder = useCallback(() => {
    void pollerRef.current?.run(true)
  }, [])

  // (Going on now, or just over: read, so that its end is shown too; the
  // view in front again: read.)
  const wasLive = useRef(live)
  useEffect(() => {
    if (live !== wasLive.current) void pollerRef.current?.run(false)
    wasLive.current = live
  }, [live])
  const wasActive = useRef(active)
  useEffect(() => {
    if (active && !wasActive.current) void pollerRef.current?.run(false)
    wasActive.current = active
  }, [active])
  return { ...shown, growing: Date.now() - shown.grewAt < GROWING, read, readOlder }
}
