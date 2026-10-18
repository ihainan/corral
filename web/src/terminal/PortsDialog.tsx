import { useEffect, useState, type FormEvent } from 'react'
import { ExternalLink, Trash2 } from 'lucide-react'
import { api, ApiError, errorText, type Forward, type ListeningPort } from '../api'
import { t } from '../i18n'
import { Dialog, useToast } from '../components/ui'

// openForward opens a port of a host in a new tab. The tab is opened during the
// tap (so it is not treated as a pop-up) and pointed at the address once known.
export async function openForward(hostId: number, port: number, path = '/', toast?: (m: string, e?: boolean) => void) {
  const win = window.open('about:blank', '_blank')
  try {
    const f = await api<Forward>('POST', '/api/forwards', { host_id: hostId, port })
    const url = f.url + '?return=' + encodeURIComponent(path.startsWith('/') ? path : '/')
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
  const [forwards, setForwards] = useState<Forward[]>([])
  const [manual, setManual] = useState('')

  const loadForwards = () =>
    api<Forward[]>('GET', '/api/forwards').then((fs) => setForwards(fs.filter((f) => f.host_id === hostId))).catch(() => {})
  useEffect(() => {
    api<ListeningPort[]>('GET', `/api/hosts/${hostId}/ports`)
      .then(setPorts)
      .catch((e) => setPortsError(e instanceof ApiError && e.code === 'no_connection' ? t('ports.needsConnection') : t('ports.loadFailed')))
    loadForwards()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hostId])

  const open = async (port: number) => {
    if (await openForward(hostId, port, '/', toast)) loadForwards()
  }
  const submit = (e: FormEvent) => {
    e.preventDefault()
    const n = Number(manual)
    if (n >= 1 && n <= 65535) void open(n)
  }

  return (
    <Dialog title={t('ports.title')} onClose={onClose}>
      <p className="hint">{t('ports.intro')}</p>
      <section className="section">
        <h2>{t('ports.listening')}</h2>
        {ports === null && !portsError && <p className="empty-inline">{t('common.loading')}</p>}
        {portsError && <p className="hint">{portsError}</p>}
        {ports && ports.length === 0 && <p className="empty-inline">{t('ports.none')}</p>}
        {ports && ports.length > 0 && (
          <div className="list">
            {ports.map((p) => (
              <div className="item port-item" key={p.port}>
                <div className="item-main" style={{ cursor: 'default' }}>
                  <span className="item-title"><span className="mono">{p.port}</span></span>
                  <span className="item-sub mono">{p.address}</span>
                </div>
                <button className="btn" onClick={() => open(p.port)}><ExternalLink size={15} />{t('ports.open')}</button>
              </div>
            ))}
          </div>
        )}
        <form className="row" onSubmit={submit} style={{ alignItems: 'center' }}>
          <input className="input mono" inputMode="numeric" aria-label={t('ports.manual')} placeholder={t('ports.manual')} value={manual}
            onChange={(e) => setManual(e.target.value.replace(/\D/g, '').slice(0, 5))} />
          <button className="btn" style={{ flex: 'none' }} disabled={!manual}>{t('ports.open')}</button>
        </form>
      </section>
      {forwards.length > 0 && (
        <section className="section">
          <h2>{t('ports.opened')}</h2>
          <div className="list">
            {forwards.map((f) => (
              <div className="item port-item" key={f.id}>
                <div className="item-main" style={{ cursor: 'default' }}>
                  <span className="item-title"><span className="mono">{f.port}</span></span>
                  <select className="select port-share" aria-label={t('ports.opened') + ' ' + f.port} value={f.sharing}
                    onChange={async (e) => {
                      await api('PUT', '/api/forwards/' + f.id, { sharing: e.target.value }).catch((err) => toast(errorText(err), true))
                      loadForwards()
                    }}>
                    <option value="private">{t('ports.sharePrivate')}</option>
                    <option value="users">{t('ports.shareUsers')}</option>
                  </select>
                </div>
                <button className="icon-btn" aria-label={t('ports.open') + ' ' + f.port} onClick={() => open(f.port)}><ExternalLink size={16} /></button>
                <button className="icon-btn" aria-label={t('ports.remove') + ' ' + f.port}
                  onClick={async () => { await api('DELETE', '/api/forwards/' + f.id).catch(() => {}); loadForwards() }}>
                  <Trash2 size={16} />
                </button>
              </div>
            ))}
          </div>
        </section>
      )}
      <div className="dialog-actions"><button className="btn" onClick={onClose}>{t('common.close')}</button></div>
    </Dialog>
  )
}
