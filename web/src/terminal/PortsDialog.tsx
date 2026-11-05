import { useEffect, useRef, useState, type FormEvent } from 'react'
import { ExternalLink, Globe, RotateCw, Trash2 } from 'lucide-react'
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

  // (Asked again with "refresh" — a server started after the dialog opened —
  // pages-1105 2.5: an answer to an older asking is dropped.)
  const portsSeq = useRef(0)
  const [portsBusy, setPortsBusy] = useState(false)
  const loadPorts = () => {
    const n = ++portsSeq.current
    setPortsError('')
    setPortsBusy(true)
    api<ListeningPort[]>('GET', `/api/hosts/${hostId}/ports`)
      .then((l) => n === portsSeq.current && setPorts(l))
      .finally(() => n === portsSeq.current && setPortsBusy(false))
      .catch((e) =>
        n === portsSeq.current &&
        setPortsError(
          e instanceof ApiError && e.code === 'no_connection' ? t('ports.needsConnection') : e instanceof ApiError && e.code === 'busy' ? t('ports.busy') : t('ports.loadFailed'),
        ),
      )
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

  // A port being opened: once, from whichever row or the field (a slow answer
  // and a second tap opened two blank tabs — 2.5). The tab is opened in the
  // tap, before any waiting (openForward), or it counts as a pop-up.
  const openingNow = useRef(new Set<number>())
  const [opening, setOpening] = useState<number[]>([])
  const open = async (port: number) => {
    if (openingNow.current.has(port)) return
    openingNow.current.add(port)
    setOpening([...openingNow.current])
    try {
      if (await openForward(hostId, port, '/', toast)) void loadForwards()
    } finally {
      openingNow.current.delete(port)
      setOpening([...openingNow.current])
    }
  }
  const isOpening = (port: number) => opening.includes(port)
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
    <Dialog title={t('ports.title')} onClose={onClose} className="ports-dialog">
      <p className="lead">{t('ports.intro')}</p>
      <section className="section">
        <div className="ports-head">
          <h2>{t('ports.listening')}</h2>
          <button type="button" className="icon-btn" onClick={loadPorts} disabled={portsBusy} aria-label={t('ports.refresh')} title={t('ports.refresh')}>
            <RotateCw size={16} className={portsBusy ? 'spin' : undefined} />
          </button>
        </div>
        {ports === null && !portsError && <p className="loading">{t('common.loading')}</p>}
        {portsError && <LoadError text={portsError} onRetry={loadPorts} />}
        {ports && ports.length === 0 && <p className="hint">{t('ports.none')}</p>}
        {ports && ports.length > 0 && (
          <ul className="rows">
            {ports.map((p) => (
              <li className="row" key={p.port}>
                <button className="row-main" onClick={() => open(p.port)} disabled={isOpening(p.port)} aria-busy={isOpening(p.port) || undefined}>
                  <Globe className="row-icon" size={18} strokeWidth={1.75} />
                  <span className="row-title mono">{p.port}</span>
                  {isOpening(p.port) ? <span className="row-meta">{t('ports.opening')}</span> : <ExternalLink size={15} className="row-meta" aria-hidden="true" />}
                  <span className="row-sub mono">{p.address}</span>
                </button>
              </li>
            ))}
          </ul>
        )}
        {/* (Its own heading, apart from the list above — ux1101 T42; a field
            and its button as the invite's are — T41.) */}
        <h3 className="ports-sub" id="ports-manual">{t('ports.manualTitle')}</h3>
        <form className="input-row" onSubmit={submit}>
          <input className="input mono" inputMode="numeric" aria-labelledby="ports-manual" placeholder={t('ports.manual')} value={manual}
            onChange={(e) => setManual(e.target.value.replace(/\D/g, '').slice(0, 5))} />
          <button className="btn btn-primary" disabled={!manual || isOpening(Number(manual))}>{isOpening(Number(manual)) ? t('ports.opening') : t('ports.open')}</button>
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
                  <button className="icon-btn" aria-label={t('ports.open') + ' ' + f.port} title={t('ports.open')} disabled={isOpening(f.port)} onClick={() => open(f.port)}><ExternalLink size={16} /></button>
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
