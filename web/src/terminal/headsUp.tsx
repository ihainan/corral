import { useEffect, useRef, useState } from 'react'
import { Lightbulb } from 'lucide-react'
import { api } from '../api'
import { t } from '../i18n'
import { Markdown } from './markdown'

// Claude Code's note of what the person should know ("✦ Heads up · …", its
// you-should-know plugin): drawn on the terminal only, above its input. Shown
// here while the terminal shows it, as the gateway reads it (convo_headsup.go),
// its words from the plugin's store; answered in the terminal, by its keys.

export interface HeadsUpNote {
  tag: string
  kind: 'offer' | 'explained'
  line: string
  more?: string
  shown_at?: number
}

const EVERY = 15000

// useHeadsUp: the note on the pane now, read every so often while on (and
// the page is not hidden). of: "<session> <pane>", as the conversation's.
export function useHeadsUp(of: string | undefined, on: boolean): HeadsUpNote | null {
  const [note, setNote] = useState<HeadsUpNote | null>(null)
  const [sessionId, pane = ''] = (of ?? '').split(' ')
  const wanted = useRef('')
  useEffect(() => {
    const key = on && sessionId && pane ? `${sessionId} ${pane}` : ''
    if (key !== wanted.current) setNote(null) // (another pane's, or none to show)
    wanted.current = key
    if (!key) return
    let timer: ReturnType<typeof setTimeout> | undefined
    const ctl = new AbortController()
    const read = async () => {
      if (!document.hidden) {
        try {
          const a = await api<{ note: HeadsUpNote | null }>('GET', `/api/sessions/${sessionId}/conversation/headsup?${new URLSearchParams({ pane })}`, undefined, { signal: ctl.signal, timeoutMs: 15000 })
          if (!ctl.signal.aborted) setNote(a.note ?? null)
        } catch {
          // (Not read this time: as it was, till the next.)
        }
      }
      if (!ctl.signal.aborted) timer = setTimeout(read, EVERY)
    }
    void read()
    return () => {
      ctl.abort()
      clearTimeout(timer)
    }
  }, [sessionId, pane, on])
  return note
}

export function HeadsUpCard({ note, onShowTerminal }: { note: HeadsUpNote; onShowTerminal: () => void }) {
  const [open, setOpen] = useState(false)
  // (A new note starts folded.)
  const seen = useRef(note.line)
  useEffect(() => {
    if (seen.current !== note.line) {
      seen.current = note.line
      setOpen(false)
    }
  }, [note.line])
  const more = note.more?.trim()
  return (
    <div className="convo-headsup" role="note">
      <Lightbulb size={14} className="convo-headsup-mark" aria-hidden="true" />
      <div className="convo-headsup-body">
        <div className="convo-headsup-head">
          <span className="convo-headsup-tag">{note.tag || t('headsUp.tag')}</span>
          <span className="convo-headsup-line">{note.line}</span>
        </div>
        {open && more && (
          <div className="convo-headsup-more">
            <Markdown text={more} soft />
          </div>
        )}
        <div className="convo-headsup-acts">
          {more && (
            <button type="button" className="link-btn" aria-expanded={open} onClick={() => setOpen(!open)}>
              {t(open ? 'headsUp.less' : note.kind === 'explained' ? 'headsUp.answer' : 'headsUp.more')}
            </button>
          )}
          <span className="convo-headsup-hint">{t('headsUp.where')}</span>
          <button type="button" className="link-btn" onClick={onShowTerminal}>
            {t('convo.lookTerminal')}
          </button>
        </div>
      </div>
    </div>
  )
}
