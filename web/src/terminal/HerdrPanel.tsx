import { useEffect, useRef } from 'react'
import { X } from 'lucide-react'
import type { HerdrSummary } from '../api'
import { hasKey, t } from '../i18n'
import { useFocusTrap } from '../components/ui'

const statusKey = (s: string) => {
  const k = 'herdr.status.' + s
  return hasKey(k) ? t(k) : ''
}

type PanelProps = {
  summary: HerdrSummary
  onClose: () => void
  onFocus: (action: 'focus_workspace' | 'focus_pane', target: string) => void
}

// Workspaces and agents of the attached herdr session. Docked beside the
// terminal on wide screens, a modal sheet over it on phones.
export function HerdrPanel(props: PanelProps & { sheet: boolean }) {
  return props.sheet ? <HerdrSheet {...props} /> : <HerdrDock {...props} />
}

function HerdrSheet(props: PanelProps) {
  const ref = useFocusTrap<HTMLDivElement>(props.onClose)
  return (
    <div className="herdr-backdrop" onMouseDown={(e) => e.target === e.currentTarget && props.onClose()}>
      <div className="herdr-panel sheet" role="dialog" aria-modal="true" aria-label={t('herdr.title')} ref={ref}>
        <div className="handle" aria-hidden="true" />
        <PanelBody {...props} />
      </div>
    </div>
  )
}

// The docked panel takes focus when opened and gives it back on Escape.
function HerdrDock(props: PanelProps) {
  const ref = useRef<HTMLElement>(null)
  useEffect(() => {
    ref.current?.querySelector<HTMLElement>('.herdr-list button')?.focus()
  }, [])
  return (
    <aside
      className="herdr-panel"
      aria-label={t('herdr.title')}
      ref={ref}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.stopPropagation()
          props.onClose()
        }
      }}
    >
      <PanelBody {...props} />
    </aside>
  )
}

function PanelBody(props: PanelProps) {
  const s = props.summary
  return (
    <>
      <div className="herdr-head">
        <div>
          <h2 className="herdr-title">{t('herdr.title')}</h2>
          {s.waiting > 0 && <div className="herdr-waiting st-blocked">{t('herdr.waiting', { n: s.waiting })}</div>}
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
      <p className="herdr-version">herdr {s.version} · {s.session}</p>
    </>
  )
}
