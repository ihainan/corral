import { useCallback, useEffect, useState } from 'react'
import { MoreHorizontal, Pencil, Plus, Server, SquareTerminal, Trash2, X } from 'lucide-react'
import { api, errorText, type Host, type SessionInfo } from '../api'
import { relTime, t } from '../i18n'
import { linkHandler, navigate } from '../router'
import { Confirm, Menu, useToast } from '../components/ui'
import { useConnect } from '../connect'

export function HostsPage() {
  const toast = useToast()
  const [hosts, setHosts] = useState<Host[] | null>(null)
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [confirm, setConfirm] = useState<React.ReactNode>(null)

  const load = useCallback(() => {
    api<Host[]>('GET', '/api/hosts').then(setHosts).catch((e) => toast(errorText(e), true))
    api<SessionInfo[]>('GET', '/api/sessions').then(setSessions).catch(() => {})
  }, [toast])

  useEffect(() => {
    load()
    const onVisible = () => document.visibilityState === 'visible' && load()
    document.addEventListener('visibilitychange', onVisible)
    const timer = setInterval(() => document.visibilityState === 'visible' && api<SessionInfo[]>('GET', '/api/sessions').then(setSessions).catch(() => {}), 10000)
    return () => {
      document.removeEventListener('visibilitychange', onVisible)
      clearInterval(timer)
    }
  }, [load])

  const { connect, busyHost, dialogs } = useConnect((s) => navigate('/t/' + s.id))
  const running = sessions.filter((s) => s.state === 'running')

  return (
    <main className="page">
      <div className="page-head">
        <h1>{t('hosts.title')}</h1>
        <div className="spacer" />
        <a className="btn btn-primary" href="/hosts/new" onClick={linkHandler('/hosts/new')}>
          <Plus size={17} />
          {t('hosts.add')}
        </a>
      </div>

      {running.length > 0 && (
        <section className="section" aria-labelledby="running-h">
          <h2 id="running-h">{t('hosts.sessions')}</h2>
          <div className="list">
            {running.map((s) => (
              <div className="item" key={s.id}>
                <div className="item-icon"><SquareTerminal size={18} /></div>
                <a className="item-main" href={'/t/' + s.id} onClick={linkHandler('/t/' + s.id)}>
                  <span className="item-title">
                    <span>{s.host_name}</span>
                    <span className="state state-running">{t('hosts.state.running')}</span>
                  </span>
                  <span className="item-sub">
                    {s.title ? s.title + ' · ' : ''}
                    {relTime(s.created_at)}
                    {s.viewers > 0 ? ' · ' + t('hosts.viewers', { n: s.viewers }) : ''}
                  </span>
                </a>
                <div className="item-actions">
                  <button
                    className="icon-btn"
                    aria-label={t('hosts.closeSession')}
                    title={t('hosts.closeSession')}
                    onClick={() =>
                      setConfirm(
                        <Confirm
                          title={t('hosts.closeSession')}
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
                    }
                  >
                    <X size={18} />
                  </button>
                </div>
              </div>
            ))}
          </div>
        </section>
      )}

      <section className="section" aria-labelledby="hosts-h">
        {running.length > 0 && <h2 id="hosts-h">{t('hosts.title')}</h2>}
        {hosts === null ? (
          <p className="empty-inline">{t('common.loading')}</p>
        ) : hosts.length === 0 ? (
          <div className="empty">
            <Server size={28} aria-hidden="true" />
            <h2>{t('hosts.empty.title')}</h2>
            <p>{t('hosts.empty.body')}</p>
            <a className="btn btn-primary" href="/hosts/new" onClick={linkHandler('/hosts/new')}>
              <Plus size={17} />
              {t('hosts.add')}
            </a>
          </div>
        ) : (
          <div className="list">
            {hosts.map((h) => (
              <div className="item" key={h.id}>
                <div className="item-icon"><Server size={18} /></div>
                <button className="item-main" onClick={() => connect(h)} disabled={busyHost === h.id} aria-label={`${t('hosts.connect')} ${h.name}`}>
                  <span className="item-title">
                    <span>{h.name}</span>
                    {busyHost === h.id && <span className="tag">{t('hosts.connecting')}</span>}
                  </span>
                  <span className="item-sub">
                    <span className="mono">{h.username}@{h.address}{h.port !== 22 ? ':' + h.port : ''}</span>
                    {h.last_connected_at ? ' · ' + t('hosts.lastConnected', { when: relTime(h.last_connected_at) }) : ''}
                  </span>
                </button>
                <div className="item-actions">
                  <Menu
                    button={(open, toggle) => (
                      <button className="icon-btn" aria-label={`${t('common.more')}：${h.name}`} aria-expanded={open} onClick={toggle}>
                        <MoreHorizontal size={18} />
                      </button>
                    )}
                  >
                    {(close) => (
                      <>
                        <a href={`/hosts/${h.id}/edit`} onClick={(e) => { close(); linkHandler(`/hosts/${h.id}/edit`)(e) }}>
                          <Pencil size={16} />
                          {t('common.edit')}
                        </a>
                        <button
                          className="danger"
                          onClick={() => {
                            close()
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
                          }}
                        >
                          <Trash2 size={16} />
                          {t('common.delete')}
                        </button>
                      </>
                    )}
                  </Menu>
                </div>
              </div>
            ))}
          </div>
        )}
      </section>
      {confirm}
      {dialogs}
    </main>
  )
}
