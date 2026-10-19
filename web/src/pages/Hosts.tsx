import { useCallback, useEffect, useState } from 'react'
import { CircleStop, Code2, MoreHorizontal, Pencil, Plus, Server, Trash2, X, Eye } from 'lucide-react'
import { api, type Host, type SessionInfo, type SharedSession } from '../api'
import { relTime, t } from '../i18n'
import { linkHandler, navigate } from '../router'
import { Confirm, EmptyState, LoadError, Menu } from '../components/ui'
import { useConnect } from '../connect'
import { useSession } from '../app'
import { useCode } from '../terminal/CodeDialog'

export function HostsPage() {
  const [hosts, setHosts] = useState<Host[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [shared, setShared] = useState<SharedSession[]>([]) // others' sessions this user may watch
  const [sharedFailed, setSharedFailed] = useState(false)
  const [sessFailed, setSessFailed] = useState(false)
  const [confirm, setConfirm] = useState<React.ReactNode>(null)

  const refreshSessions = useCallback(() => {
    api<SharedSession[]>('GET', '/api/shared')
      .then((l) => {
        setShared(l)
        setSharedFailed(false)
      })
      .catch(() => setSharedFailed(true))
    return api<SessionInfo[]>('GET', '/api/sessions')
      .then((x) => {
        setSessions(x)
        setSessFailed(false)
      })
      .catch(() => setSessFailed(true))
  }, [])
  const load = useCallback(() => {
    api<Host[]>('GET', '/api/hosts')
      .then((h) => {
        setHosts(h)
        setFailed(false)
      })
      .catch(() => setFailed(true))
    void refreshSessions()
  }, [refreshSessions])

  useEffect(() => {
    load()
    const onVisible = () => document.visibilityState === 'visible' && load()
    document.addEventListener('visibilitychange', onVisible)
    const timer = setInterval(() => document.visibilityState === 'visible' && refreshSessions(), 10000)
    return () => {
      document.removeEventListener('visibilitychange', onVisible)
      clearInterval(timer)
    }
  }, [load, refreshSessions])

  const { connect, busyHost, dialogs } = useConnect((s) => navigate('/t/' + s.id))
  const { boot } = useSession()
  const code = useCode()
  const refreshCode = code.refresh
  useEffect(() => {
    if (boot.code && hosts) refreshCode(hosts.filter((h) => h.auth !== 'ask').map((h) => h.id))
  }, [boot.code, hosts, refreshCode])
  const running = sessions.filter((s) => s.state === 'running')
  const byHost = (id: number) => running.filter((s) => s.host_id === id).sort((a, b) => a.created_at - b.created_at)

  // The confirmation names exactly what the row showed, plus when it started,
  // so sessions without a title (or with the same one) can be told apart.
  const closeSession = (s: SessionInfo, label: string) =>
    setConfirm(
      <Confirm
        title={t('hosts.closeSessionOf', { name: `${s.host_name}：${label}（${t('hosts.startedAgo', { when: relTime(s.created_at) })} · #${s.id.slice(0, 6)}）` })}
        body={t('hosts.closeSessionConfirm')}
        confirm={t('hosts.closeSession')}
        danger
        onClose={() => setConfirm(null)}
        onConfirm={async () => {
          await api('DELETE', '/api/sessions/' + s.id)
          load()
        }}
      />,
    )
  const deleteHost = (h: Host) =>
    setConfirm(
      <Confirm
        title={t('host.delete')}
        body={t('hosts.deleteConfirm', { name: h.name })}
        confirm={t('common.delete')}
        danger
        onClose={() => setConfirm(null)}
        onConfirm={async () => {
          await api('DELETE', '/api/hosts/' + h.id)
          load()
        }}
      />,
    )

  const empty = hosts !== null && hosts.length === 0
  return (
    <main className="page">
      <div className="page-head">
        <h1>{t('hosts.title')}</h1>
        <div className="spacer" />
        {sessFailed && (
          <button className="link-btn quiet-note" onClick={refreshSessions}>{t('hosts.sessionsStale')}</button>
        )}
        {!empty && (
          <a className="btn btn-primary" href="/hosts/new" onClick={linkHandler('/hosts/new')}>
            <Plus size={16} />
            {t('hosts.add')}
          </a>
        )}
      </div>

      {failed && <LoadError onRetry={load} text={t('hosts.loadFailed')} />}
      {hosts === null && !failed && <p className="loading">{t('common.loading')}</p>}
      {empty && (
        <EmptyState
          icon={<Server size={20} strokeWidth={1.75} />}
          title={t('hosts.empty.title')}
          body={t('hosts.empty.body')}
          action={
            <a className="btn btn-primary" href="/hosts/new" onClick={linkHandler('/hosts/new')}>
              <Plus size={16} />
              {t('hosts.add')}
            </a>
          }
        />
      )}
      {hosts && hosts.length > 0 && (
        <ul className="rows">
          {hosts.map((h) => {
            const mine = byHost(h.id)
            return (
              <li key={h.id} className="row-group">
                <div className="row">
                  <button className="row-main" onClick={() => connect(h)} disabled={busyHost === h.id} aria-label={`${t('hosts.connect')} ${h.name}`} aria-describedby={'hs-' + h.id}>
                    <Server className="row-icon" size={18} strokeWidth={1.75} />
                    <span className="row-title">{h.name}</span>
                    <span className="row-meta">
                      {busyHost === h.id ? t('hosts.connecting') : h.last_connected_at ? relTime(h.last_connected_at) : ''}
                    </span>
                    <span className="row-sub" id={'hs-' + h.id}>
                      {h.username}@{h.address}
                      {h.port !== 22 ? ':' + h.port : ''}
                      {h.jump_host_id ? ' · ' + t('host.via', { name: hosts?.find((x) => x.id === h.jump_host_id)?.name ?? '?' }) : ''}
                    </span>
                  </button>
                  <div className="row-actions">
                    <Menu
                      label={h.name}
                      button={(open, toggle) => (
                        <button className="icon-btn" aria-label={`${t('common.more')}：${h.name}`} aria-expanded={open} onClick={toggle}>
                          <MoreHorizontal size={18} />
                        </button>
                      )}
                      items={() => [
                        { label: t('common.edit'), icon: <Pencil size={16} />, href: `/hosts/${h.id}/edit`, onSelect: () => navigate(`/hosts/${h.id}/edit`) },
                        boot.code && h.auth !== 'ask' ? { label: t('code.open'), icon: <Code2 size={16} />, onSelect: () => code.open(h.id, h.name) } : null,
                        boot.code && code.running[h.id] ? { label: t('code.stop'), icon: <CircleStop size={16} />, onSelect: () => code.stop(h.id) } : null,
                        { label: t('host.delete'), icon: <Trash2 size={16} />, danger: true, separator: true, onSelect: () => deleteHost(h) },
                      ]}
                    />
                  </div>
                </div>
                {mine.map((s, i) => {
                  const name = mine.length > 1 ? t('hosts.sessionN', { n: i + 1 }) : t('hosts.session')
                  return (
                    <div className="row row-session" key={s.id}>
                      <a className="row-main" href={'/t/' + s.id} onClick={linkHandler('/t/' + s.id)} aria-label={`${h.name}：${s.title || name}`} aria-describedby={'sd-' + s.id} title={s.title || undefined}>
                        <span className={'dot ' + (s.meta?.waiting ? 'warn' : 'ok')} aria-hidden="true" />
                        <span className="row-title">{s.title || name}</span>
                        <span className="row-meta" title={new Date(s.created_at * 1000).toLocaleString()}>{t('hosts.startedAgo', { when: relTime(s.created_at) })}</span>
                        <span className="row-sub" id={'sd-' + s.id}>{sessionDetail(s)}</span>
                      </a>
                      <div className="row-actions">
                        <button className="icon-btn" aria-label={t('hosts.closeNamed', { name: `${h.name}：${s.title || name}` })} title={t('hosts.closeSession')} onClick={() => closeSession(s, s.title || name)}>
                          <X size={16} />
                        </button>
                      </div>
                    </div>
                  )
                })}
              </li>
            )
          })}
        </ul>
      )}
      {(shared.length > 0 || sharedFailed) && (
        <section className="section shared-sessions">
          <h2>{t('watch.sharedTitle')}</h2>
          {sharedFailed && <LoadError text={t('watch.sharedFailed')} onRetry={() => void refreshSessions()} />}
          <ul className="rows">
            {shared.map((s) => (
              <li className="row row-session" key={s.id}>
                <a className="row-main" href={'/w/' + s.id} onClick={linkHandler('/w/' + s.id)}>
                  <Eye className="row-icon" size={18} strokeWidth={1.75} />
                  <span className="row-title">{s.host_name}{s.title ? ' · ' + s.title : ''}</span>
                  <span className="row-meta" title={new Date(s.created_at * 1000).toLocaleString()}>{t('hosts.startedAgo', { when: relTime(s.created_at) })}</span>
                  <span className="row-sub">{t('watch.sharedBy', { owner: s.owner_name })}</span>
                </a>
              </li>
            ))}
          </ul>
        </section>
      )}
      {confirm}
      {dialogs}
      {code.dialog}
    </main>
  )
}

// The line under a running session: what herdr reports (where you are, who
// is waiting), otherwise how it is being used.
function sessionDetail(s: SessionInfo): string {
  const parts: string[] = []
  const m = s.meta
  if (m && m.t === 'herdr') {
    const focused = m.workspaces.find((w) => w.focused)
    if (focused) parts.push(t('hosts.herdrAt', { name: focused.label }))
    parts.push(t('hosts.herdrSpaces', { n: m.workspaces.length }))
    const agents = m.workspaces.reduce((n, w) => n + w.agents.length, 0)
    if (agents > 0) parts.push(t('hosts.herdrAgents', { n: agents }))
    if (m.waiting > 0) parts.push(t('hosts.herdrWaiting', { n: m.waiting }))
  } else {
    parts.push(s.persistence === 'tmux' ? 'tmux · ' + (s.mux_session ?? '') : s.persistence === 'herdr' ? 'herdr' : t('host.persistNone'))
  }
  parts.push(s.viewers > 0 ? t('hosts.viewers', { n: s.viewers }) : t('hosts.noViewers'))
  parts.push(`${s.cols}×${s.rows}`)
  return parts.join(' · ')
}
