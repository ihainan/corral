import { useEffect, useRef, useState } from 'react'
import { Plus, SquareTerminal, Layers, LayoutGrid, CornerDownRight } from 'lucide-react'
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
// sessions, a new tmux session, or a plain shell.
export function OpenPicker(props: { host: Host; found: HostChoices; busy: boolean; onClose: () => void; onPick: (c: OpenChoice) => void }) {
  const { host, found } = props
  const c = found.choose
  const last = lastChoice(host.id)
  const lastRef = useRef<HTMLButtonElement | null>(null)
  const [picked, setPicked] = useState<OpenChoice | null>(null) // being opened
  useEffect(() => {
    lastRef.current?.focus()
  }, [])

  const item = (choice: OpenChoice, icon: React.ReactNode, title: string, sub?: string) => {
    const isLast = same(last, choice)
    const opening = props.busy && same(picked, choice)
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
          {icon}
          <span className="pick-main">
            <span className="pick-title">{title}</span>
            {opening ? <span className="pick-sub">{t('hosts.connecting')}</span> : sub && <span className="pick-sub">{sub}</span>}
          </span>
          {isLast && <span className="pick-last">{t('picker.last')}</span>}
        </button>
      </li>
    )
  }

  return (
    <Dialog title={t('picker.title', { name: host.name })} onClose={props.onClose} className="picker">
      {found.running.length > 0 && (
        <section className="pick-group" aria-labelledby="pick-running">
          <h3 id="pick-running">{t('picker.running')}</h3>
          <ul>
            {found.running.map((r) => {
              const what = r.kind === 'shell' ? t('picker.shell') : r.kind + ' · ' + (r.name || t('picker.herdrDefault'))
              const when = t('hosts.startedAgo', { when: relTime(r.created_at) })
              return (
                <li key={r.id}>
                  <button type="button" className="pick-item" disabled={props.busy} onClick={() => navigate('/t/' + r.id)}>
                    <CornerDownRight size={18} className="pick-icon" />
                    <span className="pick-main">
                      <span className="pick-title">{r.title || what}</span>
                      <span className="pick-sub">{r.title ? what + ' · ' + when : when}</span>
                    </span>
                  </button>
                </li>
              )
            })}
          </ul>
        </section>
      )}
      {c.herdr_sessions.length > 0 && (
        <section className="pick-group" aria-labelledby="pick-herdr">
          <h3 id="pick-herdr">herdr</h3>
          <ul>
            {c.herdr_sessions.map((s) =>
              item({ kind: 'herdr', name: s.name }, <LayoutGrid size={18} className="pick-icon" />, s.name, s.default ? t('picker.herdrDefault') : undefined),
            )}
          </ul>
        </section>
      )}
      {(c.tmux || c.unknown) && (
        <section className="pick-group" aria-labelledby="pick-tmux">
          <h3 id="pick-tmux">tmux</h3>
          <ul>
            {c.tmux_sessions.map((s) =>
              item(
                { kind: 'tmux', name: s.name },
                <Layers size={18} className="pick-icon" />,
                s.name,
                t('picker.windows', { n: s.windows }) + (s.attached ? ' · ' + t('picker.attached') : ''),
              ),
            )}
            {item({ kind: 'tmux_new' }, <Plus size={18} className="pick-icon" />, t('picker.tmuxNew'), t('picker.tmuxNewHint'))}
          </ul>
        </section>
      )}
      <section className="pick-group" aria-labelledby="pick-other">
        <h3 id="pick-other">{t('picker.other')}</h3>
        <ul>{item({ kind: 'shell' }, <SquareTerminal size={18} className="pick-icon" />, t('picker.shell'), t('picker.shellHint'))}</ul>
      </section>
      {c.unknown && <p className="hint">{t('picker.unknown')}</p>}
      {props.busy && (
        <p className="sr-only" role="status">
          {t('hosts.connecting')}
        </p>
      )}
      <div className="dialog-actions">
        <button type="button" className="btn" onClick={props.onClose}>
          {t('common.cancel')}
        </button>
      </div>
    </Dialog>
  )
}
