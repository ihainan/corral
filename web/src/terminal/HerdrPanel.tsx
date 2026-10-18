import { X } from 'lucide-react'
import type { HerdrSummary } from '../api'
import { hasKey, t } from '../i18n'

const statusKey = (s: string) => {
  const k = 'herdr.status.' + s
  return hasKey(k) ? t(k) : ''
}

// Workspaces and agents of the attached herdr session. Docked beside the
// terminal on wide screens, a sheet over it on phones.
export function HerdrPanel(props: {
  summary: HerdrSummary
  onClose: () => void
  onFocus: (action: 'focus_workspace' | 'focus_pane', target: string) => void
}) {
  const s = props.summary
  return (
    <aside className="herdr-panel" aria-label={t('herdr.title')}>
      <div className="herdr-head">
        <div>
          <div className="herdr-title">{t('herdr.title')}</div>
          <div className="hint">
            {s.waiting > 0 ? <span className="st-blocked">{t('herdr.waiting', { n: s.waiting })}</span> : `herdr ${s.version} · ${s.session}`}
          </div>
        </div>
        <button className="icon-btn" onClick={props.onClose} aria-label={t('common.close')}><X size={18} /></button>
      </div>
      <ul className="herdr-list">
        {s.workspaces.map((w) => (
          <li key={w.id} className={w.focused ? 'focused' : undefined}>
            <button className="herdr-ws" onClick={() => props.onFocus('focus_workspace', w.id)}>
              <span className="herdr-num">{w.number}</span>
              <span className="herdr-label">{w.label}</span>
              <span className={'herdr-status st-' + w.status}>{statusKey(w.status)}</span>
            </button>
            {w.agents.length > 0 && (
              <ul className="herdr-agents">
                {w.agents.map((a) => (
                  <li key={a.pane_id}>
                    <button className="herdr-agent" onClick={() => props.onFocus('focus_pane', a.pane_id)}>
                      <span className={'dot st-' + a.status} aria-hidden="true" />
                      <span className="herdr-agent-main">
                        <span className="herdr-agent-name">{a.agent}</span>
                        {a.title && <span className="herdr-agent-title">{a.title}</span>}
                      </span>
                      <span className={'herdr-status st-' + a.status}>{statusKey(a.status)}</span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </li>
        ))}
      </ul>
      {s.workspaces.every((w) => w.agents.length === 0) && <p className="hint herdr-empty">{t('herdr.empty')}</p>}
    </aside>
  )
}
