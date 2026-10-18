import { useEffect, useRef, useState, type FormEvent } from 'react'
import { MoreHorizontal, UserPlus } from 'lucide-react'
import { api, errorText, type AdminUser, type AuditEntry } from '../api'
import { relTime, t } from '../i18n'
import { useSession } from '../app'
import { Confirm, CopyButton, Dialog, Field, LoadError, Menu, useToast } from '../components/ui'

export function AdminUsersPage() {
  const { me } = useSession()
  const toast = useToast()
  const [users, setUsers] = useState<AdminUser[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [dialog, setDialog] = useState<React.ReactNode>(null)
  const load = () =>
    api<AdminUser[]>('GET', '/api/admin/users')
      .then((u) => {
        setUsers(u)
        setFailed(false)
      })
      .catch(() => setFailed(true))
  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const update = async (u: AdminUser, patch: Partial<Pick<AdminUser, 'role' | 'disabled'>>) => {
    try {
      await api('PUT', '/api/admin/users/' + u.id, patch)
      load()
    } catch (e) {
      toast(errorText(e), true)
    }
  }

  return (
    <main className="page">
      <div className="page-head">
        <h1>{t('admin.users')}</h1>
        <div className="spacer" />
        <button className="btn btn-primary" onClick={() => setDialog(<InviteDialog onClose={() => setDialog(null)} />)}>
          <UserPlus size={16} />
          {t('admin.invite')}
        </button>
      </div>
      {failed && users !== null && <LoadError onRetry={load} />}
      {users === null && failed ? (
        <LoadError onRetry={load} />
      ) : users === null ? (
        <p className="empty-inline">{t('common.loading')}</p>
      ) : (
        <div className="list">
          {users.map((u) => (
            <div className="item" key={u.id}>
              <div className="item-main" style={{ cursor: 'default' }}>
                <span className="item-title">
                  <span>{u.display_name || u.username}</span>
                  {u.id === me?.id && <span className="tag">（{t('admin.you')}）</span>}
                </span>
                <span className="item-sub">
                  <span className="mono">{u.username}</span>
                  {' · '}
                  {u.role === 'admin' ? t('admin.roleAdmin') : t('admin.roleMember')}
                  {u.open_sessions > 0 ? ' · ' + t('admin.openSessions', { n: u.open_sessions }) : ''}
                  {u.disabled && <span className="state state-lost">{' · ' + t('admin.disabled')}</span>}
                </span>
              </div>
              <div className="item-actions">
                <Menu
                  button={(open, toggle) => (
                    <button className="icon-btn" aria-label={`${t('common.more')}：${u.username}`} aria-expanded={open} onClick={toggle}>
                      <MoreHorizontal size={18} />
                    </button>
                  )}
                >
                  {(close) => (
                    <>
                      <button onClick={() => { close(); update(u, { role: u.role === 'admin' ? 'member' : 'admin' }) }}>
                        {u.role === 'admin' ? t('admin.makeMember') : t('admin.makeAdmin')}
                      </button>
                      <button onClick={() => { close(); setDialog(<ResetDialog user={u} onClose={() => setDialog(null)} />) }}>
                        {t('admin.resetPassword')}
                      </button>
                      <hr />
                      {u.disabled ? (
                        <button onClick={() => { close(); update(u, { disabled: false }) }}>{t('admin.enable')}</button>
                      ) : (
                        <button
                          className="danger"
                          onClick={() => {
                            close()
                            setDialog(
                              <Confirm
                                title={t('admin.disable')}
                                body={t('admin.disableConfirm', { name: u.display_name || u.username })}
                                confirm={t('admin.disable')}
                                danger
                                onClose={() => setDialog(null)}
                                onConfirm={async () => {
                                  await api('PUT', '/api/admin/users/' + u.id, { disabled: true })
                                  load()
                                }}
                              />,
                            )
                          }}
                        >
                          {t('admin.disable')}
                        </button>
                      )}
                    </>
                  )}
                </Menu>
              </div>
            </div>
          ))}
        </div>
      )}
      {dialog}
    </main>
  )
}

function InviteDialog({ onClose }: { onClose: () => void }) {
  const [role, setRole] = useState<'member' | 'admin'>('member')
  const [note, setNote] = useState('')
  const [ttl, setTtl] = useState(72)
  const [link, setLink] = useState<{ url: string; expires_at: number } | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    try {
      setLink(await api('POST', '/api/admin/invites', { role, note, ttl_hours: ttl }))
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  return (
    <Dialog title={t('admin.invite')} onClose={onClose}>
      {link ? (
        <>
          <p className="lead">{t('admin.inviteLink', { when: new Date(link.expires_at * 1000).toLocaleString() })}</p>
          <div className="pubkey"><code>{link.url}</code><CopyButton text={link.url} /></div>
          <div className="dialog-actions"><button className="btn btn-primary" onClick={onClose}>{t('common.close')}</button></div>
        </>
      ) : (
        <form className="form" onSubmit={submit}>
          <div className="field">
            <span className="field-label" id="inv-role">{t('admin.inviteRole')}</span>
            <div className="seg" role="group" aria-labelledby="inv-role">
              <button type="button" aria-pressed={role === 'member'} onClick={() => setRole('member')}>{t('admin.roleMember')}</button>
              <button type="button" aria-pressed={role === 'admin'} onClick={() => setRole('admin')}>{t('admin.roleAdmin')}</button>
            </div>
          </div>
          <Field label={t('admin.inviteNote')} htmlFor="inv-note" optional>
            <input id="inv-note" className="input" value={note} onChange={(e) => setNote(e.target.value)} />
          </Field>
          <Field label={t('admin.inviteTtl')} htmlFor="inv-ttl">
            <select id="inv-ttl" className="select" value={ttl} onChange={(e) => setTtl(Number(e.target.value))}>
              <option value={24}>{t('admin.inviteHours', { n: 24 })}</option>
              <option value={72}>{t('admin.inviteDays', { n: 3 })}</option>
              <option value={168}>{t('admin.inviteDays', { n: 7 })}</option>
            </select>
          </Field>
          {error && <p className="err">{error}</p>}
          <div className="dialog-actions">
            <button type="button" className="btn" onClick={onClose}>{t('common.cancel')}</button>
            <button className="btn btn-primary" disabled={busy}>{t('admin.inviteCreate')}</button>
          </div>
        </form>
      )}
    </Dialog>
  )
}

function ResetDialog({ user, onClose }: { user: AdminUser; onClose: () => void }) {
  const toast = useToast()
  const [pw, setPw] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    try {
      await api('POST', `/api/admin/users/${user.id}/password`, { password: pw })
      toast(t('settings.saved'))
      onClose()
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  return (
    <Dialog title={t('admin.resetPassword')} onClose={onClose}>
      <form className="form" onSubmit={submit}>
        <p className="lead">{t('admin.resetPasswordBody', { name: user.display_name || user.username })}</p>
        <Field label={t('settings.newPassword')} htmlFor="rp-pw" hint={t('setup.passwordHint')} error={error}>
          <input id="rp-pw" className="input" type="password" autoComplete="new-password" minLength={10} value={pw} onChange={(e) => setPw(e.target.value)} required />
        </Field>
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={onClose}>{t('common.cancel')}</button>
          <button className="btn btn-primary" disabled={busy}>{t('admin.resetPassword')}</button>
        </div>
      </form>
    </Dialog>
  )
}

export function AdminAuditPage() {
  const toast = useToast()
  const [entries, setEntries] = useState<AuditEntry[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [more, setMore] = useState(true)
  const [loading, setLoading] = useState(false)
  const busy = useRef(false)
  const gen = useRef(0)
  const load = async (before?: number) => {
    if (busy.current) return
    busy.current = true
    setLoading(true)
    const mine = ++gen.current
    try {
      const rows = await api<AuditEntry[]>('GET', '/api/admin/audit' + (before ? '?before=' + before : ''))
      if (mine !== gen.current) return
      setEntries((prev) => {
        if (!before || !prev) return rows
        const seen = new Set(prev.map((e) => e.id))
        return [...prev, ...rows.filter((e) => !seen.has(e.id))]
      })
      setMore(rows.length === 100)
      setFailed(false)
    } catch (e) {
      if (before) toast(errorText(e), true)
      else setFailed(true)
    } finally {
      busy.current = false
      setLoading(false)
    }
  }
  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  return (
    <main className="page">
      <div className="page-head"><h1>{t('admin.audit')}</h1></div>
      {entries === null && failed ? (
        <LoadError onRetry={() => load()} />
      ) : entries === null ? (
        <p className="empty-inline">{t('common.loading')}</p>
      ) : entries.length === 0 ? (
        <p className="empty-inline">{t('admin.auditEmpty')}</p>
      ) : (
        <>
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>{t('audit.time')}</th>
                  <th>{t('audit.user')}</th>
                  <th>{t('audit.action')}</th>
                  <th>{t('audit.target')}</th>
                  <th>{t('audit.ip')}</th>
                </tr>
              </thead>
              <tbody>
                {entries.map((e) => (
                  <tr key={e.id}>
                    <td className="nowrap" title={new Date(e.ts * 1000).toLocaleString()}>{relTime(e.ts)}</td>
                    <td className="nowrap">{e.username || '—'}</td>
                    <td className="nowrap mono">{e.action}</td>
                    <td>{e.target}{Object.keys(e.detail ?? {}).length > 0 && <div className="hint mono">{JSON.stringify(e.detail)}</div>}</td>
                    <td className="nowrap mono">{e.ip}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {more && <div><button className="btn" disabled={loading} onClick={() => load(entries[entries.length - 1].id)}>{t('admin.loadMore')}</button></div>}
        </>
      )}
    </main>
  )
}
