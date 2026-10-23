import { useEffect, useMemo, useState } from 'react'
import { MessageCircleReply, MessagesSquare, SquareTerminal } from 'lucide-react'
import { api, type HerdrAgent, type SessionInfo } from '../api'
import { t, type I18nKey } from '../i18n'
import { navigate } from '../router'
import { Dialog, useToast } from '../components/ui'
import { ReplyDialog } from '../terminal/ReplyDialog'
import { ConversationView, type ConvoAgent } from '../terminal/Conversation'
import { useConversation } from '../terminal/conversation'
import { sessionWhich } from '../terminal/sessionName'

// Every agent of the sessions the gateway has open, in one list: the ones
// waiting for you first. It holds what herdr reports for the herdr
// sessions that are open (open on the gateway: a closed browser does not
// close them). tmux sessions and plain shells are listed apart, as herdr's
// state is all there is to go by. From a row: answer an agent waiting,
// read its conversation (Claude Code), or open its terminal at its pane.

const POLL_MS = 3000
const ORDER: Status[] = ['blocked', 'working', 'done', 'idle', 'unknown']
type Status = 'blocked' | 'working' | 'done' | 'idle' | 'unknown'

interface Row {
  s: SessionInfo
  a: HerdrAgent
  ws: string
  status: Status
  last?: string // (herdr lost: what it said last)
}

const claude = (name?: string) => !!name && /claude/i.test(name)

export function AgentsPage() {
  const [sessions, setSessions] = useState<SessionInfo[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [reply, setReply] = useState<{ s: SessionInfo; a: HerdrAgent } | null>(null)
  const [convo, setConvo] = useState<Row | null>(null)

  useEffect(() => {
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const load = () => {
      timer = undefined
      if (document.hidden) return
      api<SessionInfo[]>('GET', '/api/sessions')
        .then((l) => {
          if (stopped) return
          setSessions(l.filter((x) => x.state === 'running'))
          setFailed(false)
        })
        .catch(() => !stopped && setFailed(true))
        .finally(() => {
          if (!stopped) timer = setTimeout(load, POLL_MS)
        })
    }
    const onVisible = () => {
      if (!document.hidden && !timer) load()
    }
    document.addEventListener('visibilitychange', onVisible)
    load()
    return () => {
      stopped = true
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [])

  const { rows, others } = useMemo(() => {
    const rows: Row[] = []
    const others: SessionInfo[] = []
    for (const s of sessions ?? []) {
      const m = s.meta
      if (s.persistence !== 'herdr' || !m || m.t !== 'herdr') {
        if (s.persistence === 'tmux' || s.persistence === 'herdr') others.push(s)
        continue
      }
      for (const w of m.workspaces) {
        for (const a of w.agents) {
          const known = (ORDER as string[]).includes(a.status) ? (a.status as Status) : 'unknown'
          rows.push(m.lost ? { s, a, ws: w.label, status: 'unknown', last: known === 'unknown' ? undefined : known } : { s, a, ws: w.label, status: known })
        }
      }
    }
    rows.sort((x, y) => ORDER.indexOf(x.status) - ORDER.indexOf(y.status) || x.s.host_name.localeCompare(y.s.host_name) || x.ws.localeCompare(y.ws))
    return { rows, others }
  }, [sessions])

  const all = sessions ?? []
  const groups = ORDER.map((st) => ({ st, rows: rows.filter((r) => r.status === st) })).filter((g) => g.rows.length > 0)

  return (
    <div className="page agents-page">
      <div className="page-head">
        <h1>{t('agents.title')}</h1>
      </div>
      <p className="hint agents-scope">{t('agents.scope')}</p>
      {failed && <p className="hint agents-failed" role="status">{t('agents.failed')}</p>}
      {sessions === null ? (
        <p className="hint">{t('common.loading')}</p>
      ) : rows.length === 0 ? (
        <p className="agents-empty">{t('agents.none')}</p>
      ) : (
        groups.map((g) => (
          <section key={g.st} className="agents-group" aria-labelledby={'agents-' + g.st}>
            <h2 id={'agents-' + g.st} className={'agents-group-title is-' + g.st}>
              {t(`agents.group.${g.st}` as I18nKey)}
              <span className="agents-group-n">{g.rows.length}</span>
            </h2>
            <ul className="agents-list">
              {g.rows.map((r) => (
                <li key={r.s.id + ' ' + r.a.pane_id} className="agents-row">
                  <div className="agents-row-text">
                    <span className="agents-row-title">{r.a.title || r.a.agent}</span>
                    <span className="agents-row-where">
                      {[r.s.host_name, sessionWhich(r.s, all), r.ws, claude(r.a.agent) ? '' : r.a.agent].filter(Boolean).join(' · ')}
                      {r.last && ' · ' + t('agents.last', { status: t(`convo.status.${r.last}` as I18nKey) })}
                    </span>
                  </div>
                  <div className="agents-row-actions">
                    {r.status === 'blocked' && (
                      <button type="button" className="btn btn-primary" onClick={() => setReply({ s: r.s, a: r.a })}>
                        <MessageCircleReply size={16} aria-hidden="true" />
                        {t('agents.answer')}
                      </button>
                    )}
                    {claude(r.a.agent) && (
                      <button type="button" className="btn" onClick={() => setConvo(r)}>
                        <MessagesSquare size={16} aria-hidden="true" />
                        {t('agents.read')}
                      </button>
                    )}
                    <button type="button" className="btn btn-ghost" onClick={() => navigate(`/t/${r.s.id}?pane=${encodeURIComponent(r.a.pane_id)}`)}>
                      <SquareTerminal size={16} aria-hidden="true" />
                      {t('agents.open')}
                    </button>
                  </div>
                </li>
              ))}
            </ul>
          </section>
        ))
      )}
      {others.length > 0 && (
        <section className="agents-group" aria-labelledby="agents-others">
          <h2 id="agents-others" className="agents-group-title">{t('agents.others')}</h2>
          <p className="hint">{t('agents.othersHint')}</p>
          <ul className="agents-list">
            {others.map((s) => (
              <li key={s.id} className="agents-row">
                <div className="agents-row-text">
                  <span className="agents-row-title">{[s.host_name, sessionWhich(s, all)].join(' · ')}</span>
                  {s.title && <span className="agents-row-where">{s.title}</span>}
                </div>
                <div className="agents-row-actions">
                  <button type="button" className="btn btn-ghost" onClick={() => navigate('/t/' + s.id)}>
                    <SquareTerminal size={16} aria-hidden="true" />
                    {t('agents.open')}
                  </button>
                </div>
              </li>
            ))}
          </ul>
        </section>
      )}
      {reply && <ReplyDialog key={reply.s.id + reply.a.pane_id} sessionId={reply.s.id} agent={reply.a} onClose={() => setReply(null)} />}
      {convo && (
        <ConvoDialog
          row={convo}
          onClose={() => setConvo(null)}
          onReply={() => {
            setConvo(null)
            setReply({ s: convo.s, a: convo.a })
          }}
        />
      )}
    </div>
  )
}

// A conversation read from the list: of that pane, whatever pane is in
// front in its herdr (the gateway reads it by pane). Reading only: messages
// are typed in its terminal.
function ConvoDialog({ row, onClose, onReply }: { row: Row; onClose: () => void; onReply: () => void }) {
  const toast = useToast()
  const convo = useConversation(row.s.id, true, row.a.pane_id)
  const agent: ConvoAgent = { where: row.ws, title: row.a.title, status: row.status }
  const open = () => navigate(`/t/${row.s.id}?pane=${encodeURIComponent(row.a.pane_id)}`)
  return (
    <Dialog title={[row.s.host_name, row.a.title || row.a.agent].filter(Boolean).join(' · ')} onClose={onClose} className="agents-convo">
      <div className="convo-host">
        <ConversationView
          convo={convo}
          agent={agent}
          pending={[]}
          onReply={row.status === 'blocked' && convo.status === 'ready' ? onReply : undefined}
          onDismiss={() => {}}
          onShowTerminal={open}
          onCopied={() => toast(t('common.copied'))}
        />
      </div>
      <div className="dialog-actions">
        <button type="button" className="btn btn-ghost" onClick={onClose}>{t('common.close')}</button>
        <button type="button" className="btn btn-primary" onClick={open}>{t('agents.openHere')}</button>
      </div>
    </Dialog>
  )
}
