import { useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { ConvoTool } from './conversation'

// What a session's subagents do now, those they sent off in turn with them
// (its log tells only of its own): looked at on the host every so often while
// any of its own is out, more often, with what each does, while the list is
// opened; not while the page is hidden. See convo_subscan.go.

export interface SubScanAgent {
  id: string
  what?: string
  depth?: number // (2 and on: sent off by a subagent)
  ts?: string
  tool?: ConvoTool
  tokens?: number
}

const BRIEF_EVERY = 15000
const DETAIL_EVERY = 5000

export function useSubScan(of: string | undefined, on: boolean, detail: boolean): SubScanAgent[] {
  const [list, setList] = useState<SubScanAgent[]>([])
  const [sessionId, pane = ''] = (of ?? '').split(' ')
  const wanted = useRef('')
  useEffect(() => {
    const key = on && sessionId ? `${sessionId} ${pane}` : ''
    if (key !== wanted.current) setList([]) // (another's, or none out any more)
    wanted.current = key
    if (!key) return
    let timer: ReturnType<typeof setTimeout> | undefined
    const ctl = new AbortController()
    const read = async () => {
      if (!document.hidden) {
        try {
          const q = new URLSearchParams({ pane, ...(detail ? { detail: '1' } : {}) })
          const a = await api<{ agents: SubScanAgent[] }>('GET', `/api/sessions/${sessionId}/conversation/subagents?${q}`, undefined, { signal: ctl.signal, timeoutMs: 20000 })
          if (!ctl.signal.aborted) setList(a.agents ?? [])
        } catch {
          // (Not read this time: as it was, till the next.)
        }
      }
      if (!ctl.signal.aborted) timer = setTimeout(read, detail ? DETAIL_EVERY : BRIEF_EVERY)
    }
    void read()
    return () => {
      ctl.abort()
      clearTimeout(timer)
    }
  }, [sessionId, pane, on, detail])
  return list
}
