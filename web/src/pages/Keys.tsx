import { useEffect, useRef, useState, type DragEvent, type FormEvent } from 'react'
import { Check, ChevronRight, Copy, FileUp, Import, KeyRound, MoreHorizontal, Plus, Trash2 } from 'lucide-react'
import { api, ApiError, errorText, type KeyCred } from '../api'
import { relTime, t } from '../i18n'
import { Confirm, CopyButton, Dialog, EmptyState, Field, LoadError, Menu, copyText } from '../components/ui'

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

  const generate = () => setDialog(<GenerateDialog onClose={() => setDialog(null)} onDone={() => load()} />)
  const importKey = () => setDialog(<ImportDialog onClose={() => setDialog(null)} onDone={load} />)

  return (
    <main className="page">
      <div className="page-head">
        <h1>{t('keys.title')}</h1>
        <div className="spacer" />
        {/* (With their icons, as the other pages' buttons here — ux1101 PG-41; in
            the empty state instead while there are none, as on the hosts page.) */}
        {!(keys && keys.length === 0) && (
          <>
            <button className="btn" onClick={importKey}>
              <Import size={16} />
              {t('keys.import')}
            </button>
            <button className="btn" onClick={generate}>
              <Plus size={16} />
              {t('keys.generate')}
            </button>
          </>
        )}
      </div>
      <p className="lead page-lead">{t('keys.intro')}</p>
      {failed && <LoadError onRetry={load} />}
      {keys === null && !failed && <p className="loading">{t('common.loading')}</p>}
      {keys && keys.length === 0 && (
        <EmptyState
          icon={<KeyRound size={20} strokeWidth={1.75} />}
          title={t('keys.empty')}
          body={t('keys.emptyBody')}
          action={
            <div className="btn-row">
              <button className="btn btn-primary" onClick={generate}>
                <Plus size={16} />
                {t('keys.generate')}
              </button>
              <button className="btn" onClick={importKey}>
                <Import size={16} />
                {t('keys.import')}
              </button>
            </div>
          }
        />
      )}
      {keys && keys.length > 0 && (
        <ul className="rows">
          {keys.map((k) => (
            <li key={k.id} className="row-group">
              <div className="row">
                {/* (A press shows the whole key: the chevron says so, as a host's row connects — ux1101 PG-13.) */}
                <button className="row-main has-chev" onClick={() => setOpen(open === k.id ? null : k.id)} aria-expanded={open === k.id}>
                  <KeyRound className="row-icon" size={18} strokeWidth={1.75} />
                  <span className="row-title">{k.name}</span>
                  <span className="row-meta">{relTime(k.created_at)}</span>
                  <span className="row-sub key-short">{shortKey(k.public_key)}</span>
                  <ChevronRight className="row-chev" size={16} aria-hidden="true" />
                </button>
                <div className="row-actions">
                  <CopyKey text={k.public_key} name={k.name} />
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

// The row's copy button, named for its key (not a bare "Copy" read out
// in a list of them — ux1101 PG-15).
function CopyKey({ text, name }: { text: string; name: string }) {
  const [done, setDone] = useState(false)
  const label = t('keys.copyPublicNamed', { name })
  return (
    <button
      type="button"
      className="icon-btn"
      aria-label={label}
      title={t('keys.copyPublic')}
      onClick={async () => {
        if (await copyText(text)) {
          setDone(true)
          setTimeout(() => setDone(false), 1500)
        }
      }}
    >
      {done ? <Check size={16} /> : <Copy size={16} />}
    </button>
  )
}

function GenerateDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [made, setMade] = useState<KeyCred | null>(null)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    try {
      setMade(await api<KeyCred>('POST', '/api/credentials', { name, mode: 'generate' }))
      onDone()
    } catch (err) {
      setError(errorText(err))
      setBusy(false)
    }
  }
  // Made: its public key, to put on a server (ux1101 PG-14).
  if (made)
    return (
      <Dialog title={t('keys.generate')} onClose={onClose} icon={<KeyRound size={18} />}>
        <p className="lead">{t('keys.generatedLead')}</p>
        <div className="pubkey">
          <code>{made.public_key}</code>
        </div>
        <div className="dialog-actions" ref={(el) => el?.querySelector<HTMLElement>('button')?.focus()}>
          <CopyButton text={made.public_key} label={t('keys.copyPublic')} />
          <button className="btn btn-primary" onClick={onClose}>{t('common.close')}</button>
        </div>
      </Dialog>
    )
  return (
    // (Not closed by a stray press with a name typed or the key being made: ux1101 PG-03.)
    <Dialog title={t('keys.generate')} onClose={onClose} busy={busy} guard={!!name.trim()} icon={<KeyRound size={18} />}>
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

// A key file larger than this is not one (an RSA 4096 key in PEM is some 3.3 KB).
const KEY_FILE_MAX = 64 << 10

// (Also the host form's, which selects the key imported: it is given it.)
export function ImportDialog({ onClose, onDone }: { onClose: () => void; onDone: (k: KeyCred) => void }) {
  const [name, setName] = useState('')
  const [pk, setPk] = useState('')
  const [phrase, setPhrase] = useState('')
  const [error, setError] = useState<{ field?: string; text: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const [over, setOver] = useState(false)
  const picker = useRef<HTMLInputElement>(null)
  // (Files are dropped with a mouse: on a phone, nothing to say of it.)
  const canDrop = window.matchMedia('(pointer: fine)').matches
  // A key file read here, in the browser (doc UI item 11, 11-25): its words put in the box, as
  // if pasted, and sent as those are; its name the key's, where none is typed. Any file may be
  // picked — a key's has no extension (id_ed25519) —; one too big, or a public key, is said so.
  // (The last file picked only: one read slower before it does not come after it.)
  const reading = useRef(0)
  const readKey = async (f: File | undefined) => {
    if (!f) return
    const n = ++reading.current
    if (f.size > KEY_FILE_MAX) return setError({ field: 'private_key', text: t('keys.fileTooBig') })
    let text: string
    try {
      text = await f.text()
    } catch {
      if (n === reading.current) setError({ field: 'private_key', text: t('keys.fileUnreadable') })
      return
    }
    if (n !== reading.current) return
    // (A security key's too: sk-ssh-ed25519@openssh.com.)
    if (/^(ssh-|ecdsa-|sk-)[A-Za-z0-9@.-]+ [A-Za-z0-9+/=]+/.test(text.trim())) return setError({ field: 'private_key', text: t('keys.filePublic') })
    setError(null)
    setPk(text)
    setName((v) => (v.trim() ? v : f.name.replace(/\.(pem|key|ppk)$/i, ''))) // (as it is now: typed meanwhile, kept)
  }
  const onDrop = (e: DragEvent) => {
    setOver(false)
    if (!e.dataTransfer.files.length) return // (words dragged in: the box's own)
    e.preventDefault()
    void readKey(e.dataTransfer.files[0])
  }
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    try {
      onDone(await api<KeyCred>('POST', '/api/credentials', { name, mode: 'import', private_key: pk, passphrase: phrase }))
      onClose()
    } catch (err) {
      setError({ field: err instanceof ApiError ? err.field : undefined, text: errorText(err) })
      setBusy(false)
    }
  }
  return (
    <Dialog title={t('keys.import')} onClose={onClose} busy={busy} guard={!!(name.trim() || pk.trim() || phrase)} icon={<Import size={18} />}>
      <form className="form" onSubmit={submit}>
        <Field label={t('keys.name')} htmlFor="ki-name" error={error?.field === 'name' ? error.text : undefined}>
          <input id="ki-name" className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder={t('keys.namePlaceholder')} required />
        </Field>
        <div className="key-field">
          <Field label={t('keys.privateKey')} htmlFor="ki-key" error={error?.field === 'private_key' ? error.text : undefined} hint={canDrop ? t('keys.dropHint') : undefined}>
            <textarea
              id="ki-key"
              className={'textarea' + (over ? ' is-drop' : '')}
              value={pk}
              onChange={(e) => setPk(e.target.value)}
              onDragOver={(e) => {
                if (![...e.dataTransfer.types].includes('Files')) return
                e.preventDefault()
                setOver(true)
              }}
              onDragLeave={() => setOver(false)}
              onDrop={onDrop}
              placeholder={t('keys.privateKeyPlaceholder')}
              spellCheck={false}
              autoCapitalize="none"
              autoCorrect="off"
              required
            />
          </Field>
          {/* (Beside the label, not in the hint's place: still there when an error is.) */}
          <button type="button" className="btn btn-ghost key-file-btn" onClick={() => picker.current?.click()}>
            <FileUp size={14} aria-hidden="true" />
            {t('keys.fromFile')}
          </button>
          <input
            ref={picker}
            type="file"
            hidden
            onChange={(e) => {
              void readKey(e.target.files?.[0])
              e.target.value = '' // (the same file picked again reads again)
            }}
          />
        </div>
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
