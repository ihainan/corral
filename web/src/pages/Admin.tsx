import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import { ChevronRight, KeyRound, KeySquare, MoreHorizontal, ScrollText, Search, Send, Shield, Smartphone, UserCheck, UserPlus, UserX } from 'lucide-react'
import { api, ApiError, errorText, type AdminUser, type AuditEntry } from '../api'
import { t } from '../i18n'
import { PhoneBack, useSession } from '../app'
import { auditAction, auditDetail, auditTarget, byDay, clockText, sessionHosts, spanFrom, whenText, type DaySpan } from './pageText'
import { Confirm, CopyButton, Dialog, EmptyState, Field, LoadError, Menu, useToast } from '../components/ui'

type UserFilter = 'all' | 'admin' | 'disabled' | 'no2fa'
const USER_FILTERS: UserFilter[] = ['all', 'admin', 'disabled', 'no2fa']
const USER_SEARCH_FROM = 8 // (users: more than this, and the search shows)

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

  // Finding one among many (pages-1105 2.7): a search past a few, and who is
  // an admin, turned off, without two-step sign-in — the choice stays (with
  // the count it leaves) when a change takes the row out of it.
  const [q, setQ] = useState('')
  const [only, setOnly] = useState<UserFilter>('all')
  const needle = q.trim().toLowerCase()
  const shownUsers = (users ?? []).filter(
    (u) =>
      (!needle || u.username.toLowerCase().includes(needle) || u.display_name.toLowerCase().includes(needle)) &&
      (only === 'all' || (only === 'admin' ? u.role === 'admin' : only === 'disabled' ? u.disabled : !u.totp)),
  )
  const filtering = needle !== '' || only !== 'all'
  const findable = (users?.length ?? 0) > USER_SEARCH_FROM || filtering

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
        <PhoneBack />
        <h1>{t('admin.users')}</h1>
        <div className="spacer" />
        <button className="btn" onClick={() => setDialog(<CreateUserDialog onClose={() => setDialog(null)} onCreated={load} />)}>
          <UserPlus size={16} />
          {t('admin.create')}
        </button>
        <button className="btn" onClick={() => setDialog(<InviteDialog onClose={() => setDialog(null)} />)}>
          <Send size={16} />
          {t('admin.invite')}
        </button>
      </div>
      {failed && <LoadError onRetry={load} />}
      {users === null ? (
        !failed && <p className="loading">{t('common.loading')}</p>
      ) : (
        <>
        {findable && (
          <div className="page-tools">
            <label className="page-search">
              <Search size={16} aria-hidden="true" />
              <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder={t('admin.searchUsers')} aria-label={t('admin.searchUsers')} />
            </label>
            <div className="seg page-order" role="group" aria-label={t('admin.filterUsers')}>
              {USER_FILTERS.map((f) => (
                <button key={f} type="button" aria-pressed={only === f} onClick={() => setOnly(f)}>
                  {t(`admin.only.${f}` as const)}
                </button>
              ))}
            </div>
          </div>
        )}
        {filtering && (
          <p className="page-count" role="status">
            {t('admin.shownOf', { n: shownUsers.length, all: users.length })}{' '}
            <button type="button" className="link-btn" onClick={() => (setQ(''), setOnly('all'))}>{t('common.clearSearch')}</button>
          </p>
        )}
        <ul className="rows">
          {shownUsers.map((u) => (
            <li className="row" key={u.id}>
              <div className="row-main no-icon">
                <span className="row-title">
                  {u.display_name || u.username}
                  {u.id === me?.id && <span className="you">{t('admin.you')}</span>}
                </span>
                {u.role === 'admin' && <span className="row-meta">{t('admin.roleAdmin')}</span>}
                {(() => {
                  // The user name again only when the title is another name;
                  // every row with a second line, so rows are one height (ux1101 PG-38).
                  const parts: React.ReactNode[] = []
                  if (u.display_name && u.display_name !== u.username) parts.push(u.username)
                  if (u.disabled) parts.push(<span key="d" className="state-danger">{t('admin.disabled')}</span>)
                  if (u.open_sessions > 0) parts.push(t('admin.openSessions', { n: u.open_sessions }))
                  if (u.totp) parts.push(t('admin.twoStep'))
                  parts.push(t('admin.createdAt', { when: whenText(u.created_at) }))
                  return <span className="row-sub">{parts.flatMap((x, i) => (i ? [' · ', x] : [x]))}</span>
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
                    // One's own role and account are not one's own to take
                    // away here: off, saying why (the server would allow it —
                    // ux1101 PG-08). Another's role, once confirmed.
                    u.id === me?.id
                      ? { label: u.role === 'admin' ? t('admin.makeMember') : t('admin.makeAdmin'), icon: <Shield size={16} />, disabled: true, hint: t('admin.selfRoleHint') }
                      : {
                          label: u.role === 'admin' ? t('admin.makeMember') : t('admin.makeAdmin'),
                          icon: <Shield size={16} />,
                          onSelect: () =>
                            setDialog(
                              <Confirm
                                title={u.role === 'admin' ? t('admin.makeMember') : t('admin.makeAdmin')}
                                body={t(u.role === 'admin' ? 'admin.roleToMemberBody' : 'admin.roleToAdminBody', { name: u.display_name || u.username })}
                                confirm={u.role === 'admin' ? t('admin.makeMember') : t('admin.makeAdmin')}
                                onClose={() => setDialog(null)}
                                onConfirm={async () => {
                                  await api('PUT', '/api/admin/users/' + u.id, { role: u.role === 'admin' ? 'member' : 'admin' })
                                  load()
                                }}
                              />,
                            ),
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
                      : u.id === me?.id
                      ? { label: t('admin.disable'), icon: <UserX size={16} />, separator: true, disabled: true, hint: t('admin.selfDisableHint') }
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
        </>
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
    // (A note typed or the link being made is not lost to a stray press; the
    // link, shown once, leaves by its button — ux1101 PG-03.)
    <Dialog title={t('admin.invite')} onClose={onClose} busy={busy} guard={!!note.trim() || !!link} icon={<Send size={18} />}>
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
          <Field label={t('admin.inviteNote')} htmlFor="inv-note" optional hint={t('admin.inviteNoteHint')}>
            <input id="inv-note" className="input" value={note} onChange={(e) => setNote(e.target.value)} />
          </Field>
          <Field label={t('admin.inviteTtl')} htmlFor="inv-ttl">
            <select id="inv-ttl" className="select" value={ttl} onChange={(e) => setTtl(Number(e.target.value))}>
              <option value={24}>{t('admin.inviteHours', { n: 24 })}</option>
              <option value={72}>{t('admin.inviteDays', { n: 3 })}</option>
              <option value={168}>{t('admin.inviteDays', { n: 7 })}</option>
            </select>
          </Field>
          {error && <p className="err" role="alert">{error}</p>}
          <div className="dialog-actions">
            <button type="button" className="btn" onClick={onClose} disabled={busy}>{t('common.cancel')}</button>
            <button className="btn btn-primary" disabled={busy}>{t('admin.inviteCreate')}</button>
          </div>
        </form>
      )}
    </Dialog>
  )
}

// A first password for an account made here, from an alphabet without look-alikes.
function newPassword(): string {
  const abc = 'abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789'
  return Array.from(crypto.getRandomValues(new Uint32Array(14)), (n) => abc[n % abc.length]).join('')
}

// An account made by the administrator, without an invite: its user name, a
// first password (shown once afterwards, to hand over) which the user
// replaces at the first sign-in unless that is turned off.
function CreateUserDialog({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [username, setUsername] = useState('')
  const [name, setName] = useState('')
  const [role, setRole] = useState<'member' | 'admin'>('member')
  const [pw, setPw] = useState(newPassword)
  const [mustChange, setMustChange] = useState(true)
  const [done, setDone] = useState<{ username: string; password: string } | null>(null)
  const [error, setError] = useState<{ field?: string; text: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setError(null)
    try {
      await api('POST', '/api/admin/users', { username: username.trim(), display_name: name.trim(), password: pw, role, must_change: mustChange })
      setDone({ username: username.trim(), password: pw })
      onCreated()
    } catch (err) {
      const code = err instanceof ApiError ? err.code : ''
      setError({ field: code === 'bad_username' || code === 'username_taken' ? 'username' : code === 'weak_password' ? 'password' : undefined, text: errorText(err) })
      setBusy(false)
    }
  }
  const fe = (f: string) => (error?.field === f ? error.text : undefined)
  if (done) {
    const text = t('admin.createdText', { username: done.username, password: done.password })
    return (
      // (The first password is shown this once: only its button leaves, as the recovery codes — ux1101 PG-04.)
      <Dialog title={t('admin.create')} onClose={onClose} guard icon={<UserPlus size={18} />}>
        <p className="lead">{mustChange ? t('admin.createdBody') : t('admin.createdBodyKeep')}</p>
        <div className="pubkey"><code className="pre-line">{text}</code><CopyButton text={text} /></div>
        <div className="dialog-actions"><button className="btn btn-primary" onClick={onClose}>{t('common.close')}</button></div>
      </Dialog>
    )
  }
  return (
    <Dialog title={t('admin.create')} onClose={onClose} busy={busy} guard={!!username.trim() || !!name.trim()} icon={<UserPlus size={18} />}>
      <form className="form" onSubmit={submit} noValidate>
        <p className="lead">{t('admin.createLead')}</p>
        <Field label={t('login.username')} htmlFor="cu-user" hint={t('admin.usernameHint')} error={fe('username')}>
          <input id="cu-user" className="input" value={username} onChange={(e) => setUsername(e.target.value)} autoCapitalize="none" autoCorrect="off" spellCheck={false} autoComplete="off" required aria-invalid={!!fe('username')} />
        </Field>
        <Field label={t('setup.displayName')} htmlFor="cu-name" optional>
          <input id="cu-name" className="input" value={name} maxLength={64} onChange={(e) => setName(e.target.value)} />
        </Field>
        <div className="field">
          <span className="field-label" id="cu-role">{t('admin.inviteRole')}</span>
          <div className="seg" role="group" aria-labelledby="cu-role">
            <button type="button" aria-pressed={role === 'member'} onClick={() => setRole('member')}>{t('admin.roleMember')}</button>
            <button type="button" aria-pressed={role === 'admin'} onClick={() => setRole('admin')}>{t('admin.roleAdmin')}</button>
          </div>
        </div>
        <Field label={t('admin.firstPassword')} htmlFor="cu-pw" hint={t('admin.firstPasswordHint')} error={fe('password')}>
          <div className="input-row">
            <input id="cu-pw" className="input mono" value={pw} onChange={(e) => setPw(e.target.value)} autoComplete="off" autoCapitalize="none" autoCorrect="off" spellCheck={false} required aria-invalid={!!fe('password')} />
            <button type="button" className="btn" onClick={() => setPw(newPassword())}>{t('admin.generate')}</button>
          </div>
        </Field>
        <label className="check">
          <input type="checkbox" checked={mustChange} onChange={(e) => setMustChange(e.target.checked)} />
          <span>{t('admin.mustChange')}</span>
        </label>
        {error && !error.field && <p className="err" role="alert">{error.text}</p>}
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={onClose} disabled={busy}>{t('common.cancel')}</button>
          <button className="btn btn-primary" disabled={busy || !username.trim() || !pw}>{t('admin.createSubmit')}</button>
        </div>
      </form>
    </Dialog>
  )
}

function ResetDialog({ user, onClose }: { user: AdminUser; onClose: () => void }) {
  const toast = useToast()
  const [pw, setPw] = useState('')
  const [mustChange, setMustChange] = useState(true)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    try {
      await api('POST', `/api/admin/users/${user.id}/password`, { password: pw, must_change: mustChange })
      toast(t('settings.saved'))
      onClose()
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  return (
    <Dialog title={t('admin.resetPassword')} onClose={onClose} busy={busy} guard={!!pw} icon={<KeyRound size={18} />}>
      <form className="form" onSubmit={submit}>
        <p className="lead">{t('admin.resetPasswordBody', { name: user.display_name || user.username })}</p>
        <Field label={t('settings.newPassword')} htmlFor="rp-pw" hint={t('setup.passwordHint')} error={error}>
          <input id="rp-pw" className="input" type="password" autoComplete="new-password" minLength={8} value={pw} onChange={(e) => setPw(e.target.value)} required />
        </Field>
        <label className="check">
          <input type="checkbox" checked={mustChange} onChange={(e) => setMustChange(e.target.checked)} />
          <span>{t('admin.mustChange')}</span>
        </label>
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={onClose} disabled={busy}>{t('common.cancel')}</button>
          <button className="btn btn-primary" disabled={busy}>{t('admin.resetPassword')}</button>
        </div>
      </form>
    </Dialog>
  )
}

type AuditKind = '' | 'account' | 'hosts' | 'sessions' | 'agents' | 'other'
const AUDIT_KINDS: AuditKind[] = ['', 'account', 'hosts', 'sessions', 'agents', 'other']
type AuditFilter = { user: number; kind: AuditKind; span: DaySpan }

export function AdminAuditPage() {
  const toast = useToast()
  const [entries, setEntries] = useState<AuditEntry[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [more, setMore] = useState(true)
  const [loading, setLoading] = useState(false)
  // Narrowed (pages-1105 3.2) by who did it, the kind of action, the days
  // back; the gateway narrows the whole log. A new choice starts the list
  // again: what an older one was still reading is dropped when it comes.
  const [filter, setFilter] = useState<AuditFilter>({ user: 0, kind: '', span: 'all' })
  const [users, setUsers] = useState<AdminUser[]>([])
  useEffect(() => {
    api<AdminUser[]>('GET', '/api/admin/users').then(setUsers, () => {})
  }, [])
  const busy = useRef(false)
  const gen = useRef(0)
  const load = async (before?: number, f = filter) => {
    if (before && busy.current) return
    busy.current = true
    setLoading(true)
    const mine = ++gen.current
    try {
      const q = new URLSearchParams()
      if (before) q.set('before', String(before))
      if (f.user) q.set('user', String(f.user))
      if (f.kind) q.set('kind', f.kind)
      const from = spanFrom(f.span)
      if (from) q.set('from', String(from))
      const rows = await api<AuditEntry[]>('GET', '/api/admin/audit' + (q.toString() ? '?' + q : ''))
      if (mine !== gen.current) return
      setEntries((prev) => {
        if (!before || !prev) return rows
        const seen = new Set(prev.map((e) => e.id))
        return [...prev, ...rows.filter((e) => !seen.has(e.id))]
      })
      setMore(rows.length === 100)
      setFailed(false)
    } catch (e) {
      if (mine !== gen.current) return
      if (before) toast(errorText(e), true)
      else setFailed(true)
    } finally {
      if (mine === gen.current) {
        busy.current = false
        setLoading(false)
      }
    }
  }
  const narrow = (f: AuditFilter) => {
    setFilter(f)
    setEntries(null)
    setFailed(false)
    void load(undefined, f)
  }
  const filtered = filter.user !== 0 || filter.kind !== '' || filter.span !== 'all'
  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  const hosts = useMemo(() => sessionHosts(entries ?? []), [entries])
  return (
    <main className="page">
      <div className="page-head">
        <PhoneBack />
        <h1>{t('admin.audit')}</h1>
      </div>
      <div className="page-tools audit-tools">
        <select className="select" value={filter.user} onChange={(e) => narrow({ ...filter, user: Number(e.target.value) })} aria-label={t('audit.byUser')}>
          <option value={0}>{t('audit.allUsers')}</option>
          {users.map((u) => <option key={u.id} value={u.id}>{u.display_name && u.display_name !== u.username ? `${u.display_name}（${u.username}）` : u.username}</option>)}
        </select>
        <select className="select" value={filter.kind} onChange={(e) => narrow({ ...filter, kind: e.target.value as AuditKind })} aria-label={t('audit.byKind')}>
          {AUDIT_KINDS.map((k) => <option key={k} value={k}>{t(`audit.kind.${k || 'all'}` as const)}</option>)}
        </select>
        <select className="select" value={filter.span} onChange={(e) => narrow({ ...filter, span: e.target.value as DaySpan })} aria-label={t('audit.bySpan')}>
          {(['all', 'today', '7d', '30d'] as const).map((sp) => <option key={sp} value={sp}>{t(`span.${sp}` as const)}</option>)}
        </select>
      </div>
      {entries === null && failed ? (
        <LoadError onRetry={() => load()} />
      ) : entries === null ? (
        <p className="loading">{t('common.loading')}</p>
      ) : entries.length === 0 && filtered ? (
        <p className="page-none">
          {t('audit.noMatch')}{' '}
          <button type="button" className="link-btn" onClick={() => narrow({ user: 0, kind: '', span: 'all' })}>{t('common.clearFilters')}</button>
        </p>
      ) : entries.length === 0 ? (
        <EmptyState icon={<ScrollText size={20} strokeWidth={1.75} />} title={t('admin.auditEmpty')} />
      ) : (
        <>
          {/* Actions in words, details as "key: value", a session by its
              host where this page knows it, dates past a day (ux1101 PG-06/09/10). */}
          <table className="audit-table">
            <colgroup>
              {/* (A time of day: the day is the heading above — pages-1105 3.2.) */}
              <col style={{ width: '5em' }} />
              <col style={{ width: '12%' }} />
              <col style={{ width: '20%' }} />
              <col />
              <col style={{ width: '14%' }} />
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
            {byDay(entries, (e) => e.ts).map((g) => (
            <tbody key={g.key}>
              <tr className="audit-day">
                <th colSpan={5} scope="colgroup">{g.heading}</th>
              </tr>
              {g.items.map((e) => {
                const detail = auditDetail(e.detail)
                const target = auditTarget(e.target, hosts)
                return (
                  <tr key={e.id}>
                    <td className="tnum" title={new Date(e.ts * 1000).toLocaleString()}>{clockText(e.ts)}</td>
                    <td title={e.username}>{e.username || '—'}</td>
                    <td title={e.action}>{auditAction(e.action)}</td>
                    <td title={[e.target, ...auditDetail(e.detail, true)].filter(Boolean).join('\n')}>
                      {target || '—'}
                      {detail.length > 0 && <span className="detail">{detail.join(' · ')}</span>}
                    </td>
                    <td className="code" title={e.ip}>{e.ip}</td>
                  </tr>
                )
              })}
            </tbody>
            ))}
          </table>
          <div className="audit-list">
            {byDay(entries, (e) => e.ts).map((g) => (
            <section className="day-group" key={g.key} aria-label={g.heading}>
            <h2 className="day-head">{g.heading}</h2>
            {g.items.map((e) => {
              const target = auditTarget(e.target, hosts)
              return (
                <details className="audit-item" key={e.id}>
                  <summary>
                    <span className="top">
                      <span className="action">{auditAction(e.action)}</span>
                      <span className="row-meta tnum">{clockText(e.ts)}</span>
                      <ChevronRight className="audit-chev" size={16} aria-hidden="true" />
                    </span>
                    <span className="hint">
                      {e.username || '—'}
                      {target ? ' · ' + target : ''}
                    </span>
                  </summary>
                  <pre>{[new Date(e.ts * 1000).toLocaleString(), e.ip, target !== e.target ? e.target : '', e.action, ...auditDetail(e.detail, true)].filter(Boolean).join('\n')}</pre>
                </details>
              )
            })}
            </section>
            ))}
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
