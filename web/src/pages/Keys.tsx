import { useEffect, useState, type FormEvent } from 'react'
import { KeyRound, MoreHorizontal, Trash2 } from 'lucide-react'
import { api, ApiError, errorText, type KeyCred } from '../api'
import { relTime, t } from '../i18n'
import { Confirm, CopyButton, Dialog, EmptyState, Field, LoadError, Menu } from '../components/ui'

// "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5…Jk0q4"
function shortKey(k: string) {
  const [type, body] = k.split(' ')
  if (!body || body.length < 24) return k
  return `${type} ${body.slice(0, 16)}…${body.slice(-6)}`
}

export function KeysPage() {
  const [keys, setKeys] = useState<KeyCred[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [open, setOpen] = useState<number | null>(null)
  const [dialog, setDialog] = useState<React.ReactNode>(null)
  const load = () =>
    api<KeyCred[]>('GET', '/api/credentials')
      .then((k) => {
        setKeys(k)
        setFailed(false)
      })
      .catch(() => setFailed(true))
  useEffect(() => {
    load()
  }, [])

  const generate = () => setDialog(<GenerateDialog onClose={() => setDialog(null)} onDone={load} />)
  const importKey = () => setDialog(<ImportDialog onClose={() => setDialog(null)} onDone={load} />)

  return (
    <main className="page">
      <div className="page-head">
        <h1>{t('keys.title')}</h1>
        <div className="spacer" />
        <button className="btn" onClick={importKey}>{t('keys.import')}</button>
        <button className="btn btn-primary" onClick={generate}>{t('keys.generate')}</button>
      </div>
      <p className="lead">{t('keys.intro')}</p>
      {failed && <LoadError onRetry={load} />}
      {keys === null && !failed && <p className="loading">{t('common.loading')}</p>}
      {keys && keys.length === 0 && <EmptyState icon={<KeyRound size={20} strokeWidth={1.75} />} title={t('keys.empty')} body={t('keys.emptyBody')} />}
      {keys && keys.length > 0 && (
        <ul className="rows">
          {keys.map((k) => (
            <li key={k.id} className="row-group">
              <div className="row">
                <button className="row-main" onClick={() => setOpen(open === k.id ? null : k.id)} aria-expanded={open === k.id}>
                  <KeyRound className="row-icon" size={18} strokeWidth={1.75} />
                  <span className="row-title">{k.name}</span>
                  <span className="row-meta">{relTime(k.created_at)}</span>
                  <span className="row-sub key-short">{shortKey(k.public_key)}</span>
                </button>
                <div className="row-actions">
                  <CopyButton text={k.public_key} />
                  <Menu
                    label={k.name}
                    button={(o, toggle) => (
                      <button className="icon-btn" aria-label={`${t('common.more')}：${k.name}`} aria-expanded={o} onClick={toggle}>
                        <MoreHorizontal size={18} />
                      </button>
                    )}
                    items={() => [
                      {
                        label: t('common.delete'),
                        icon: <Trash2 size={16} />,
                        danger: true,
                        onSelect: () =>
                          setDialog(
                            <Confirm
                              title={t('common.delete')}
                              body={t('keys.deleteConfirm', { name: k.name })}
                              confirm={t('common.delete')}
                              danger
                              onClose={() => setDialog(null)}
                              onConfirm={async () => {
                                await api('DELETE', '/api/credentials/' + k.id)
                                load()
                              }}
                            />,
                          ),
                      },
                    ]}
                  />
                </div>
              </div>
              {open === k.id && (
                <div className="pubkey" style={{ margin: '0 8px 12px 38px' }}>
                  <code>{k.public_key}</code>
                </div>
              )}
            </li>
          ))}
        </ul>
      )}
      {dialog}
    </main>
  )
}

function GenerateDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    try {
      await api('POST', '/api/credentials', { name, mode: 'generate' })
      onDone()
      onClose()
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  return (
    <Dialog title={t('keys.generate')} onClose={onClose}>
      <form className="form" onSubmit={submit}>
        <Field label={t('keys.name')} htmlFor="k-name" error={error}>
          <input id="k-name" className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder={t('keys.namePlaceholder')} required />
        </Field>
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={onClose}>{t('common.cancel')}</button>
          <button className="btn btn-primary" disabled={busy || !name.trim()}>{t('keys.generate')}</button>
        </div>
      </form>
    </Dialog>
  )
}

function ImportDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState('')
  const [pk, setPk] = useState('')
  const [phrase, setPhrase] = useState('')
  const [error, setError] = useState<{ field?: string; text: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    try {
      await api('POST', '/api/credentials', { name, mode: 'import', private_key: pk, passphrase: phrase })
      onDone()
      onClose()
    } catch (err) {
      setError({ field: err instanceof ApiError ? err.field : undefined, text: errorText(err) })
      setBusy(false)
    }
  }
  return (
    <Dialog title={t('keys.import')} onClose={onClose}>
      <form className="form" onSubmit={submit}>
        <Field label={t('keys.name')} htmlFor="ki-name" error={error?.field === 'name' ? error.text : undefined}>
          <input id="ki-name" className="input" value={name} onChange={(e) => setName(e.target.value)} required />
        </Field>
        <Field label={t('keys.privateKey')} htmlFor="ki-key" error={error?.field === 'private_key' ? error.text : undefined}>
          <textarea id="ki-key" className="textarea" value={pk} onChange={(e) => setPk(e.target.value)} spellCheck={false} autoCapitalize="none" autoCorrect="off" required />
        </Field>
        <Field label={t('keys.passphrase')} htmlFor="ki-phrase" optional error={error?.field === 'passphrase' ? error.text : undefined}>
          <input id="ki-phrase" className="input" type="password" autoComplete="off" value={phrase} onChange={(e) => setPhrase(e.target.value)} />
        </Field>
        {error && !error.field && <p className="err">{error.text}</p>}
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={onClose}>{t('common.cancel')}</button>
          <button className="btn btn-primary" disabled={busy || !name.trim() || !pk.trim()}>{t('keys.import')}</button>
        </div>
      </form>
    </Dialog>
  )
}
