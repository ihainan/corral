import { useEffect, useState, type FormEvent } from 'react'
import { KeyRound, Trash2 } from 'lucide-react'
import { api, ApiError, errorText, type KeyCred } from '../api'
import { relTime, t } from '../i18n'
import { Confirm, CopyButton, Dialog, Field, LoadError } from '../components/ui'

export function KeysPage() {
  const [keys, setKeys] = useState<KeyCred[] | null>(null)
  const [failed, setFailed] = useState(false)
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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <main className="page">
      <div className="page-head">
        <h1>{t('keys.title')}</h1>
        <div className="spacer" />
        <button className="btn" onClick={() => setDialog(<ImportDialog onClose={() => setDialog(null)} onDone={load} />)}>{t('keys.import')}</button>
        <button className="btn btn-primary" onClick={() => setDialog(<GenerateDialog onClose={() => setDialog(null)} onDone={load} />)}>
          <KeyRound size={16} />
          {t('keys.generate')}
        </button>
      </div>
      <p className="lead">{t('keys.intro')}</p>
      {failed && keys !== null && <LoadError onRetry={load} />}
      {keys === null && failed ? (
        <LoadError onRetry={load} />
      ) : keys === null ? (
        <p className="empty-inline">{t('common.loading')}</p>
      ) : keys.length === 0 ? (
        <p className="empty-inline">{t('keys.empty')}</p>
      ) : (
        <div className="list">
          {keys.map((k) => (
            <div className="item" key={k.id} style={{ alignItems: 'flex-start' }}>
              <div className="item-icon"><KeyRound size={18} /></div>
              <div className="item-main" style={{ cursor: 'default', gap: 8 }}>
                <span className="item-title"><span>{k.name}</span><span className="tag">{relTime(k.created_at)}</span></span>
                <div className="pubkey">
                  <code>{k.public_key}</code>
                  <CopyButton text={k.public_key} />
                </div>
              </div>
              <div className="item-actions">
                <button
                  className="icon-btn"
                  aria-label={`${t('common.delete')} ${k.name}`}
                  onClick={() =>
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
                    )
                  }
                >
                  <Trash2 size={17} />
                </button>
              </div>
            </div>
          ))}
        </div>
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
          <input id="k-name" className="input" value={name} onChange={(e) => setName(e.target.value)} required />
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
