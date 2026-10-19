import { useEffect, useRef } from 'react'
import { Maximize2, Minimize2, X } from 'lucide-react'
import type { HerdrAgent, HerdrSummary } from '../api'
import { hasKey, t } from '../i18n'
import { leaveField, restoreFocus, useFocusTrap } from '../components/ui'

const statusKey = (s: string) => {
  const k = 'herdr.status.' + s
  return hasKey(k) ? t(k) : ''
}

type PanelProps = {
  summary: HerdrSummary
  // While the connection is down the summary is old: shown, but not actionable.
  stale: boolean
  onClose: (viaKeyboard: boolean) => void
  onFocus: (action: 'focus_workspace' | 'focus_pane' | 'zoom_pane' | 'unzoom', target: string) => void
  onReply: (agent: HerdrAgent) => void // answer an agent that waits for the user
}

// Workspaces and agents of the attached herdr session. Docked beside the
// terminal on wide screens, a modal sheet over it on phones.
export function HerdrPanel(props: PanelProps & { sheet: boolean }) {
  return props.sheet ? <HerdrSheet {...props} /> : <HerdrDock {...props} />
}

function HerdrSheet(props: PanelProps) {
  const ref = useFocusTrap<HTMLDivElement>(() => props.onClose(true))
  return (
    <div className="herdr-backdrop" onMouseDown={(e) => e.target === e.currentTarget && props.onClose(false)}>
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
    const prev = document.activeElement as HTMLElement | null
    const root = ref.current
    leaveField()
    root?.querySelector<HTMLElement>('.herdr-list button')?.focus()
    return () => restoreFocus(prev, root)
  }, [])
  return (
    <aside
      className="herdr-panel"
      aria-label={t('herdr.title')}
      ref={ref}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.stopPropagation()
          props.onClose(true)
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
          {props.stale ? (
            <div className="herdr-waiting">{t('herdr.stale')}</div>
          ) : (
            s.waiting > 0 && <div className="herdr-waiting st-blocked">{t('herdr.waiting', { n: s.waiting })}</div>
          )}
        </div>
        <div className="herdr-head-actions">
          <button className="icon-btn" disabled={props.stale} onClick={() => props.onFocus('unzoom', '')} aria-label={t('herdr.unzoom')} title={t('herdr.unzoom')}>
            <Minimize2 size={17} />
          </button>
          <button className="icon-btn" onClick={(e) => props.onClose(e.detail === 0)} aria-label={t('common.close')}><X size={18} /></button>
        </div>
      </div>
      <ul className={'herdr-list' + (props.stale ? ' stale' : '')}>
        {s.workspaces.map((w) => (
          <li key={w.id} className={w.focused ? 'focused' : undefined}>
            <button className="herdr-ws" disabled={props.stale} aria-current={w.focused ? 'true' : undefined} onClick={() => props.onFocus('focus_workspace', w.id)}>
              <span className="herdr-num">{w.number}</span>
              <span className="herdr-label">{w.label}</span>
              <span className={'herdr-status st-' + w.status}>{statusKey(w.status)}</span>
            </button>
            {w.agents.length > 0 && (
              <ul className="herdr-agents">
                {w.agents.map((a) => (
                  <li key={a.pane_id} className="herdr-agent-row">
                    <button className="herdr-agent" disabled={props.stale} onClick={() => props.onFocus('focus_pane', a.pane_id)}>
                      <span className={'dot st-' + a.status} aria-hidden="true" />
                      <span className="herdr-agent-main">
                        <span className="herdr-agent-name">{a.agent}</span>
                        {a.title && <span className="herdr-agent-title">{a.title}</span>}
                      </span>
                      <span className={'herdr-status st-' + a.status}>{statusKey(a.status)}</span>
                    </button>
                    <button className="icon-btn herdr-zoom" disabled={props.stale} onClick={() => props.onFocus('zoom_pane', a.pane_id)} aria-label={t('herdr.zoom', { agent: a.agent })} title={t('herdr.zoom', { agent: a.agent })}>
                      <Maximize2 size={16} />
                    </button>
                    {a.status === 'blocked' && (
                      <button className="herdr-reply" disabled={props.stale} onClick={() => props.onReply(a)} aria-label={t('reply.open', { agent: a.agent })}>
                        {t('reply.button')}
                      </button>
                    )}
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
