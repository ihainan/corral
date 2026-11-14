import { useEffect, useState } from 'react'
import { Search, SquarePlus } from 'lucide-react'
import { api, type Host } from '../api'
import { t } from '../i18n'
import { navigate } from '../router'
import { Dialog } from '../components/ui'

// "+": a host to open a new session to (the tab bar's, a phone's session
// switcher's): searched by name or address, the most recently connected first.
export function NewTabDialog(p: { busyHost: number | null; onPick: (h: Host) => void; onClose: () => void }) {
  const [hosts, setHosts] = useState<Host[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [q, setQ] = useState('')
  useEffect(() => {
    api<Host[]>('GET', '/api/hosts')
      .then((l) => setHosts([...l].sort((a, b) => b.last_connected_at - a.last_connected_at || a.name.localeCompare(b.name))))
      .catch(() => setFailed(true))
  }, [])
  const needle = q.trim().toLowerCase()
  const shown = (hosts ?? []).filter((h) => !needle || h.name.toLowerCase().includes(needle) || h.address.toLowerCase().includes(needle))
  // The host Enter opens: marked, and moved with Up and Down (ux1101 T32).
  const [sel, setSel] = useState(0)
  useEffect(() => setSel(0), [needle])
  const cur = Math.min(sel, Math.max(0, shown.length - 1))
  useEffect(() => {
    document.getElementById(`new-tab-host-${cur}`)?.scrollIntoView({ block: 'nearest' })
  }, [cur])
  return (
    <Dialog title={t('tabs.new')} onClose={p.onClose} className="new-tab" icon={<SquarePlus size={18} />}>
      <label className="new-tab-search">
        <Search size={16} aria-hidden="true" />
        <input
          type="search"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder={t('tabs.search')}
          aria-label={t('tabs.search')}
          autoFocus
          role="combobox"
          aria-expanded={shown.length > 0}
          aria-controls="new-tab-list"
          aria-activedescendant={shown.length > 0 ? `new-tab-host-${cur}` : undefined}
          onKeyDown={(e) => {
            if (e.nativeEvent.isComposing) return
            if (e.key === 'Enter' && shown.length > 0) {
              e.preventDefault()
              if (p.busyHost === null) p.onPick(shown[cur])
            } else if ((e.key === 'ArrowDown' || e.key === 'ArrowUp') && shown.length > 0) {
              e.preventDefault()
              setSel((cur + (e.key === 'ArrowDown' ? 1 : shown.length - 1)) % shown.length)
            }
          }}
        />
      </label>
      <div className="new-tab-list" id="new-tab-list" role={shown.length > 0 ? 'listbox' : undefined} aria-label={shown.length > 0 ? t('tabs.new') : undefined}>
        {failed ? (
          <p className="hint">{t('tabs.hostsFailed')}</p>
        ) : hosts === null ? (
          <p className="hint">{t('common.loading')}</p>
        ) : hosts.length === 0 ? (
          <p className="hint">{t('tabs.noHosts')}</p>
        ) : shown.length === 0 ? (
          <p className="hint">{t('tabs.noMatch')}</p>
        ) : (
          shown.map((h, i) => (
            <button
              key={h.id}
              id={`new-tab-host-${i}`}
              type="button"
              role="option"
              aria-selected={i === cur}
              tabIndex={-1}
              className={'new-tab-host' + (i === cur ? ' is-sel' : '')}
              disabled={p.busyHost !== null}
              aria-busy={p.busyHost === h.id || undefined}
              onMouseMove={() => i !== cur && setSel(i)}
              onClick={() => p.onPick(h)}
            >
              <span className="new-tab-name">{h.name}</span>
              <span className="new-tab-addr">{h.username}@{h.address}{h.port !== 22 ? ':' + h.port : ''}</span>
              {p.busyHost === h.id && <span className="new-tab-busy">{t('term.connecting')}</span>}
            </button>
          ))
        )}
      </div>
      <div className="dialog-actions">
        <button
          type="button"
          className="btn btn-ghost"
          onClick={() => {
            p.onClose()
            navigate('/')
          }}
        >
          {t('tabs.manage')}
        </button>
        <button type="button" className="btn" onClick={p.onClose}>{t('common.cancel')}</button>
      </div>
    </Dialog>
  )
}
