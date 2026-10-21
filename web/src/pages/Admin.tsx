import { useEffect, useRef, useState, type FormEvent } from 'react'
import { KeySquare, MoreHorizontal, ScrollText, Shield, Smartphone, UserCheck, UserPlus, UserRound, UserX } from 'lucide-react'
import { api, errorText, type AdminUser, type AuditEntry } from '../api'
import { relTime, t } from '../i18n'
import { useSession } from '../app'
import { Confirm, CopyButton, Dialog, EmptyState, Field, LoadError, Menu, useToast } from '../components/ui'

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
      {failed && <LoadError onRetry={load} />}
      {users === null ? (
        !failed && <p className="loading">{t('common.loading')}</p>
      ) : (
        <ul className="rows">
          {users.map((u) => (
            <li className="row" key={u.id}>
              <div className="row-main">
                <UserRound className="row-icon" size={18} strokeWidth={1.75} />
                <span className="row-title">
                  {u.display_name || u.username}
                  {u.id === me?.id && <span className="you">{t('admin.you')}</span>}
                </span>
                <span className="row-meta">{u.role === 'admin' ? t('admin.roleAdmin') : t('admin.roleMember')}</span>
                {(() => {
                  // The user name again only when the title is another name.
                  const parts: React.ReactNode[] = []
                  if (u.display_name && u.display_name !== u.username) parts.push(<span key="u" className="mono">{u.username}</span>)
                  if (u.open_sessions > 0) parts.push(t('admin.openSessions', { n: u.open_sessions }))
                  if (u.totp) parts.push(t('admin.twoStep'))
                  if (u.disabled) parts.push(<span key="d" className="state-danger">{t('admin.disabled')}</span>)
                  return parts.length > 0 && <span className="row-sub">{parts.flatMap((x, i) => (i ? [' · ', x] : [x]))}</span>
                })()}
              </div>
              <div className="row-actions">
                <Menu
                  label={u.display_name || u.username}
                  button={(open, toggle) => (
                    <button className="icon-btn" aria-label={`${t('common.more')}：${u.username}`} aria-expanded={open} onClick={toggle}>
                      <MoreHorizontal size={18} />
                    </button>
                  )}
                  items={() => [
                    {
                      label: u.role === 'admin' ? t('admin.makeMember') : t('admin.makeAdmin'),
                      icon: <Shield size={16} />,
                      onSelect: () => update(u, { role: u.role === 'admin' ? 'member' : 'admin' }),
                    },
                    {
                      label: t('admin.resetPassword'),
                      icon: <KeySquare size={16} />,
                      onSelect: () => setDialog(<ResetDialog user={u} onClose={() => setDialog(null)} />),
                    },
                    u.totp
                      ? {
                          label: t('admin.resetTwoStep'),
                          icon: <Smartphone size={16} />,
                          onSelect: () =>
                            setDialog(
                              <Confirm
                                title={t('admin.resetTwoStep')}
                                body={t('admin.resetTwoStepBody', { name: u.display_name || u.username })}
                                confirm={t('admin.resetTwoStep')}
                                onClose={() => setDialog(null)}
                                onConfirm={async () => {
                                  await api('POST', '/api/admin/users/' + u.id + '/totp/reset')
                                  load()
                                }}
                              />,
                            ),
                        }
                      : null,
                    u.disabled
                      ? { label: t('admin.enable'), icon: <UserCheck size={16} />, separator: true, onSelect: () => update(u, { disabled: false }) }
                      : {
                          label: t('admin.disable'),
                          icon: <UserX size={16} />,
                          danger: true,
                          separator: true,
                          onSelect: () =>
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
                            ),
                        },
                  ]}
                />
              </div>
            </li>
          ))}
        </ul>
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
          <input id="rp-pw" className="input" type="password" autoComplete="new-password" minLength={8} value={pw} onChange={(e) => setPw(e.target.value)} required />
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
        <p className="loading">{t('common.loading')}</p>
      ) : entries.length === 0 ? (
        <EmptyState icon={<ScrollText size={20} strokeWidth={1.75} />} title={t('admin.auditEmpty')} />
      ) : (
        <>
          <table className="audit-table">
            <colgroup>
              <col style={{ width: '14%' }} />
              <col style={{ width: '14%' }} />
              <col style={{ width: '22%' }} />
              <col />
              <col style={{ width: '16%' }} />
            </colgroup>
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
              {entries.map((e) => {
                const detail = Object.keys(e.detail ?? {}).length > 0 ? JSON.stringify(e.detail) : ''
                return (
                  <tr key={e.id}>
                    <td className="tnum" title={new Date(e.ts * 1000).toLocaleString()}>{relTime(e.ts)}</td>
                    <td title={e.username}>{e.username || '—'}</td>
                    <td className="code" title={e.action}>{e.action}</td>
                    <td title={[e.target, detail].filter(Boolean).join('\n')}>
                      {e.target || '—'}
                      {detail && <span className="detail">{detail}</span>}
                    </td>
                    <td className="code" title={e.ip}>{e.ip}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
          <div className="audit-list">
            {entries.map((e) => {
              const detail = Object.keys(e.detail ?? {}).length > 0 ? JSON.stringify(e.detail, null, 2) : ''
              return (
                <details className="audit-item" key={e.id}>
                  <summary>
                    <span className="top">
                      <span className="action">{e.action}</span>
                      <span className="row-meta">{relTime(e.ts)}</span>
                    </span>
                    <span className="hint">
                      {e.username || '—'}
                      {e.target ? ' · ' + e.target : ''}
                    </span>
                  </summary>
                  <pre>{[new Date(e.ts * 1000).toLocaleString(), e.ip, detail].filter(Boolean).join('\n')}</pre>
                </details>
              )
            })}
          </div>
          {more && (
            <div>
              <button className="btn" disabled={loading} onClick={() => load(entries[entries.length - 1].id)}>{t('admin.loadMore')}</button>
            </div>
          )}
        </>
      )}
    </main>
  )
}
