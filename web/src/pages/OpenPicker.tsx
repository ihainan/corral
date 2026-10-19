import { useEffect, useRef, useState } from 'react'
import { CornerDownRight, Layers, LayoutGrid, Plus, SquareTerminal, X } from 'lucide-react'
import type { Host, HostChoices, OpenChoice } from '../api'
import { relTime, t } from '../i18n'
import { navigate } from '../router'
import { local } from '../storage'
import { Dialog } from '../components/ui'

// The last thing opened on a host, on this device: it is shown selected the
// next time.
const lastKey = (hostId: number) => 'corral.lastOpen.' + hostId

export function rememberChoice(hostId: number, c: OpenChoice) {
  local.set(lastKey(hostId), JSON.stringify({ kind: c.kind === 'tmux_new' ? 'tmux_new' : c.kind, name: c.name ?? '' }))
}

function lastChoice(hostId: number): OpenChoice | null {
  try {
    const v = JSON.parse(local.get(lastKey(hostId)) ?? 'null')
    return v && typeof v.kind === 'string' ? (v as OpenChoice) : null
  } catch {
    return null
  }
}

const same = (a: OpenChoice | null, b: OpenChoice) => !!a && a.kind === b.kind && (a.name ?? '') === (b.name ?? '')

// What to open on a host: its terminals running in Corral, its herdr and tmux
// sessions, a new tmux session, or a plain shell. Each group is one list;
// a row is its name, with what else there is to know on the right.
export function OpenPicker(props: { host: Host; found: HostChoices; busy: boolean; onClose: () => void; onPick: (c: OpenChoice) => void }) {
  const { host, found } = props
  const c = found.choose
  const last = lastChoice(host.id)
  const lastRef = useRef<HTMLButtonElement | null>(null)
  const [picked, setPicked] = useState<OpenChoice | null>(null) // being opened
  // Focus starts on the remembered choice, else the first one (not on the
  // close button, which comes first in the dialog). After the dialog's own
  // initial focus, which runs after this component's effects.
  useEffect(() => {
    const f = requestAnimationFrame(() => (lastRef.current ?? document.querySelector<HTMLButtonElement>('.picker .pick-item:not(:disabled)'))?.focus())
    return () => cancelAnimationFrame(f)
  }, [])

  // meta: on the right of the name; sub: a line under it.
  const item = (choice: OpenChoice, icon: React.ReactNode, title: string, meta?: string, sub?: string) => {
    const isLast = same(last, choice)
    const opening = props.busy && same(picked, choice)
    const right = opening ? t('hosts.connecting') : isLast ? [meta, t('picker.last')].filter(Boolean).join(' · ') : meta
    return (
      <li key={choice.kind + ':' + (choice.name ?? '')}>
        <button
          type="button"
          className={'pick-item' + (isLast ? ' last' : '')}
          disabled={props.busy}
          ref={isLast ? lastRef : undefined}
          aria-busy={opening || undefined}
          onClick={() => {
            setPicked(choice)
            props.onPick(choice)
          }}
        >
          <span className="pick-icon">{icon}</span>
          <span className="pick-main">
            <span className="pick-title">{title}</span>
            {sub && <span className="pick-sub">{sub}</span>}
          </span>
          {right && <span className="pick-meta">{right}</span>}
        </button>
      </li>
    )
  }

  return (
    <Dialog title={t('picker.title', { name: host.name })} hideTitle onClose={props.onClose} className="picker">
      <div className="pick-head">
        <div className="pick-head-text">
          <h2>{t('picker.title', { name: host.name })}</h2>
          <span className="pick-target">
            {host.username}@{host.address}
            {host.port !== 22 ? ':' + host.port : ''}
          </span>
        </div>
        <button type="button" className="icon-btn" onClick={props.onClose} aria-label={t('common.cancel')} title={t('common.cancel')}>
          <X size={18} />
        </button>
      </div>
      {found.running.length > 0 && (
        <section className="pick-group" aria-labelledby="pick-running">
          <h3 id="pick-running">{t('picker.running')}</h3>
          <ul className="pick-list">
            {found.running.map((r) => {
              const what = r.kind === 'shell' ? t('picker.shell') : r.kind + ' · ' + (r.name || t('picker.herdrDefault'))
              const when = relTime(r.created_at)
              return (
                <li key={r.id}>
                  <button type="button" className="pick-item" disabled={props.busy} onClick={() => navigate('/t/' + r.id)}>
                    <span className="pick-icon">
                      <CornerDownRight size={16} />
                    </span>
                    <span className="pick-main">
                      <span className="pick-title">{r.title || what}</span>
                      {r.title && <span className="pick-sub">{what}</span>}
                    </span>
                    <span className="pick-meta">{when}</span>
                  </button>
                </li>
              )
            })}
          </ul>
        </section>
      )}
      {c.herdr_sessions.length > 0 && (
        <section className="pick-group" aria-labelledby="pick-herdr">
          <h3 id="pick-herdr">{t('picker.herdr')}</h3>
          <ul className="pick-list">
            {c.herdr_sessions.map((s) =>
              item({ kind: 'herdr', name: s.name }, <LayoutGrid size={16} />, s.name, s.default ? t('picker.herdrDefault') : undefined),
            )}
          </ul>
        </section>
      )}
      {(c.tmux || c.unknown) && c.tmux_sessions.length > 0 && (
        <section className="pick-group" aria-labelledby="pick-tmux">
          <h3 id="pick-tmux">{t('picker.tmux')}</h3>
          <ul className="pick-list">
            {c.tmux_sessions.map((s) =>
              item(
                { kind: 'tmux', name: s.name },
                <Layers size={16} />,
                s.name,
                t(s.windows === 1 ? 'picker.window' : 'picker.windows', { n: s.windows }) + (s.attached ? ' · ' + t('picker.attached') : ''),
              ),
            )}
          </ul>
        </section>
      )}
      <section className="pick-group" aria-labelledby="pick-new">
        <h3 id="pick-new">{t('picker.new')}</h3>
        <ul className="pick-list">
          {(c.tmux || c.unknown) && item({ kind: 'tmux_new' }, <Plus size={16} />, t('picker.tmuxNew'), undefined, t('picker.tmuxNewHint'))}
          {item({ kind: 'shell' }, <SquareTerminal size={16} />, t('picker.shell'), undefined, t('picker.shellHint'))}
        </ul>
      </section>
      {c.unknown && <p className="hint">{t('picker.unknown')}</p>}
      {props.busy && (
        <p className="sr-only" role="status">
          {t('hosts.connecting')}
        </p>
      )}
    </Dialog>
  )
}
