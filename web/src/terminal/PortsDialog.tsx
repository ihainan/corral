import { useEffect, useState, type FormEvent } from 'react'
import { ExternalLink, Globe, Trash2 } from 'lucide-react'
import { api, ApiError, errorText, type Forward, type ListeningPort } from '../api'
import { t } from '../i18n'
import { Dialog, LoadError, useToast } from '../components/ui'

// openForward opens a port of a host in a new tab. The tab is opened during the
// tap (so it is not treated as a pop-up) and pointed at the address once known.
// `hash` is appended to the redirect URL: browsers carry a fragment across
// redirects, while the server never sees it.
export async function openForward(hostId: number, port: number, path = '/', toast?: (m: string, e?: boolean) => void, hash = '') {
  const win = window.open('about:blank', '_blank')
  // The forwarded site must not be able to navigate this tab.
  if (win) win.opener = null
  try {
    const f = await api<Forward>('POST', '/api/forwards', { host_id: hostId, port })
    const url = f.url + '?return=' + encodeURIComponent(path.startsWith('/') ? path : '/') + (hash.startsWith('#') ? hash : '')
    if (win) win.location.href = url
    else window.location.href = url
    return f
  } catch (e) {
    win?.close()
    toast?.(errorText(e), true)
    return null
  }
}

export function PortsDialog({ hostId, onClose }: { hostId: number; onClose: () => void }) {
  const toast = useToast()
  const [ports, setPorts] = useState<ListeningPort[] | null>(null)
  const [portsError, setPortsError] = useState('')
  const [forwards, setForwards] = useState<Forward[] | null>(null)
  const [forwardsFailed, setForwardsFailed] = useState(false)
  const [manual, setManual] = useState('')

  const loadPorts = () => {
    setPortsError('')
    setPorts(null)
    api<ListeningPort[]>('GET', `/api/hosts/${hostId}/ports`)
      .then(setPorts)
      .catch((e) => setPortsError(e instanceof ApiError && e.code === 'no_connection' ? t('ports.needsConnection') : t('ports.loadFailed')))
  }
  const loadForwards = () =>
    api<Forward[]>('GET', '/api/forwards')
      .then((fs) => {
        setForwards(fs.filter((f) => f.host_id === hostId))
        setForwardsFailed(false)
      })
      .catch(() => setForwardsFailed(true))
  useEffect(() => {
    loadPorts()
    void loadForwards()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hostId])

  const open = async (port: number) => {
    if (await openForward(hostId, port, '/', toast)) void loadForwards()
  }
  const submit = (e: FormEvent) => {
    e.preventDefault()
    const n = Number(manual)
    if (n >= 1 && n <= 65535) void open(n)
    else toast(t('ports.badPort'), true)
  }
  const setSharing = async (f: Forward, sharing: string) => {
    try {
      await api('PUT', '/api/forwards/' + f.id, { sharing })
    } catch (err) {
      toast(errorText(err), true)
    }
    void loadForwards()
  }
  const remove = async (f: Forward) => {
    try {
      await api('DELETE', '/api/forwards/' + f.id)
    } catch (err) {
      toast(errorText(err), true)
    }
    void loadForwards()
  }

  return (
    <Dialog title={t('ports.title')} onClose={onClose}>
      <p className="hint">{t('ports.intro')}</p>
      <section className="section">
        <h2>{t('ports.listening')}</h2>
        {ports === null && !portsError && <p className="loading">{t('common.loading')}</p>}
        {portsError && <LoadError text={portsError} onRetry={loadPorts} />}
        {ports && ports.length === 0 && <p className="hint">{t('ports.none')}</p>}
        {ports && ports.length > 0 && (
          <ul className="rows">
            {ports.map((p) => (
              <li className="row" key={p.port}>
                <button className="row-main" onClick={() => open(p.port)}>
                  <Globe className="row-icon" size={18} strokeWidth={1.75} />
                  <span className="row-title mono">{p.port}</span>
                  <ExternalLink size={15} className="row-meta" aria-hidden="true" />
                  <span className="row-sub mono">{p.address}</span>
                </button>
              </li>
            ))}
          </ul>
        )}
        <form className="row2" onSubmit={submit}>
          <input className="input mono" inputMode="numeric" aria-label={t('ports.manual')} placeholder={t('ports.manual')} value={manual}
            onChange={(e) => setManual(e.target.value.replace(/\D/g, '').slice(0, 5))} />
          <button className="btn" style={{ flex: 'none', height: 'var(--h-input)' }} disabled={!manual}>{t('ports.open')}</button>
        </form>
      </section>
      {forwardsFailed && <LoadError onRetry={loadForwards} />}
      {forwards && forwards.length > 0 && (
        <section className="section">
          <h2>{t('ports.opened')}</h2>
          <ul className="rows">
            {forwards.map((f) => (
              <li className="row" key={f.id}>
                <div className="row-main no-icon">
                  <span className="row-title mono">{f.port}</span>
                  <select className="select port-share" aria-label={t('ports.shareLabel', { port: f.port })} value={f.sharing}
                    onChange={(e) => setSharing(f, e.target.value)}>
                    <option value="private">{t('ports.sharePrivate')}</option>
                    <option value="users">{t('ports.shareUsers')}</option>
                  </select>
                </div>
                <div className="row-actions">
                  <button className="icon-btn" aria-label={t('ports.open') + ' ' + f.port} title={t('ports.open')} onClick={() => open(f.port)}><ExternalLink size={16} /></button>
                  <button className="icon-btn" aria-label={t('ports.remove') + ' ' + f.port} title={t('ports.remove')} onClick={() => remove(f)}><Trash2 size={16} /></button>
                </div>
              </li>
            ))}
          </ul>
        </section>
      )}
      <div className="dialog-actions"><button className="btn" onClick={onClose}>{t('common.close')}</button></div>
    </Dialog>
  )
}
