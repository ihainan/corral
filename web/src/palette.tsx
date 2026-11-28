import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Bot, Command, Gauge, KeyRound, Plus, Server, Settings, SquareTerminal } from 'lucide-react'
import { api, type Host } from './api'
import { t } from './i18n'
import { navigate, usePath } from './router'
import { Dialog } from './components/ui'
import { useConnect } from './connect'
import { listAgents, matchQuery, rowTitle, useRunningSessions, type AgentRow } from './terminal/agents'
import { AgentIcon } from './terminal/agentIcons'
import { askNewAgent } from './terminal/newAgent'
import { openSettingsModal } from './pages/Settings'
import { useWorkbench } from './workbench'
import { composing } from './terminal/pageKeys'

// The command palette (workbench 11-23): one box to go anywhere — an agent
// by what it is doing, a host to connect to, a page, a new agent. ⌘K on a
// Mac with the workbench on (elsewhere the browser keeps Ctrl+K, as the
// agents' own search does: the rail's search button opens it there).
export const OPEN_PALETTE = 'corral:open-palette'
export const openPalette = () => window.dispatchEvent(new Event(OPEN_PALETTE))
const mac = /Mac|iPhone|iPad/.test(navigator.platform)
export const PALETTE_KEY = mac ? '⌘K' : ''

type Item = { key: string; group: 'agents' | 'hosts' | 'commands'; icon: ReactNode; title: string; sub?: string; hint?: string; tone?: string; go: () => void }

export function PaletteHost() {
  const [open, setOpen] = useState(false)
  const wb = useWorkbench()
  const wbRef = useRef(wb)
  wbRef.current = wb
  const openRef = useRef(open)
  openRef.current = open
  // (Kept while closed: a host's connection asks its questions after the palette has gone.)
  const fresh = useConnect((s) => navigate('/t/' + s.id))
  useEffect(() => {
    const onOpen = () => setOpen(true)
    const onKey = (e: KeyboardEvent) => {
      if (!mac || !wbRef.current || e.key.toLowerCase() !== 'k' || !e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || composing(e)) return
      // (Not over another dialog — settings, a form half filled: going somewhere from
      // here would leave it over the page, or drop what was typed. Itself open: closed.)
      if (!openRef.current && document.documentElement.classList.contains('modal-open')) return
      e.preventDefault()
      setOpen((v) => !v)
    }
    window.addEventListener(OPEN_PALETTE, onOpen)
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener(OPEN_PALETTE, onOpen)
      window.removeEventListener('keydown', onKey)
    }
  }, [])
  return (
    <>
      {open && <Palette onClose={() => setOpen(false)} onHost={(h) => fresh.connect(h)} />}
      {fresh.dialogs}
    </>
  )
}

const MOST = { agents: 8, hosts: 6 }

function Palette({ onClose, onHost }: { onClose: () => void; onHost: (h: Host) => void }) {
  const [q, setQ] = useState('')
  const [at, setAt] = useState(0)
  const path = usePath()
  // (The agents as the list has them: read now, as the list reads them, while open.)
  const { sessions } = useRunningSessions(true, 5000)
  const [hosts, setHosts] = useState<Host[] | null>(null)
  useEffect(() => {
    let live = true
    api<Host[]>('GET', '/api/hosts').then(
      (h) => live && setHosts(h),
      () => live && setHosts([]),
    )
    return () => {
      live = false
    }
  }, [])
  const items = useMemo((): Item[] => {
    const go = (f: () => void) => () => {
      onClose()
      f()
    }
    const rows: AgentRow[] = sessions ? listAgents(sessions).rows : []
    const titleOf = rowTitle
    const agents = rows
      .filter((r) => matchQuery(q, titleOf(r), r.s.host_name, r.ws, r.a.cwd, r.a.agent))
      .slice(0, MOST.agents)
      .map((r): Item => {
        const st = r.lost ? '' : r.status
        return {
          key: 'a ' + r.key,
          group: 'agents',
          icon: <AgentIcon program={r.a.agent} size={16} bare />,
          title: titleOf(r),
          sub: [r.s.host_name, r.ws && r.ws !== '~' ? r.ws : ''].filter(Boolean).join(' · '),
          hint: st === 'blocked' ? t('palette.waiting') : st === 'working' ? t('palette.working') : st === 'done' ? t('palette.done') : '',
          tone: st,
          go: go(() => navigate('/agents?' + new URLSearchParams({ s: r.s.id, pane: r.a.pane_id }))),
        }
      })
    const hostItems = (hosts ?? [])
      .filter((h) => matchQuery(q, h.name, h.address, h.username))
      .slice(0, MOST.hosts)
      .map((h): Item => ({
        key: 'h ' + h.id,
        group: 'hosts',
        icon: <Server size={16} strokeWidth={1.75} aria-hidden="true" />,
        title: t('palette.connect', { name: h.name }),
        sub: `${h.username}@${h.address}${h.port === 22 ? '' : ':' + h.port}`,
        go: go(() => onHost(h)),
      }))
    const commands = (
      [
      { key: 'c new', group: 'commands', icon: <Plus size={16} aria-hidden="true" />, title: t('palette.newAgent'), go: go(() => (path === '/agents' ? askNewAgent() : (askNewAgent(), navigate('/agents')))) },
      { key: 'c agents', group: 'commands', icon: <Bot size={16} strokeWidth={1.75} aria-hidden="true" />, title: t('palette.toAgents'), go: go(() => navigate('/agents')) },
      // (The terminals' page: where there are tabs, 700 and up.)
      ...(matchMedia('(min-width: 700px)').matches ? [{ key: 'c terms', group: 'commands', icon: <SquareTerminal size={16} strokeWidth={1.75} aria-hidden="true" />, title: t('palette.toTerminals'), go: go(() => navigate('/t')) }] : []),
      { key: 'c hosts', group: 'commands', icon: <Server size={16} strokeWidth={1.75} aria-hidden="true" />, title: t('palette.toHosts'), go: go(() => navigate('/')) },
      { key: 'c keys', group: 'commands', icon: <KeyRound size={16} strokeWidth={1.75} aria-hidden="true" />, title: t('palette.toKeys'), go: go(() => navigate('/keys')) },
      { key: 'c subscriptions', group: 'commands', icon: <Gauge size={16} strokeWidth={1.75} aria-hidden="true" />, title: t('palette.subscriptions'), go: go(() => openSettingsModal('subscriptions')) },
      { key: 'c settings', group: 'commands', icon: <Settings size={16} strokeWidth={1.75} aria-hidden="true" />, title: t('palette.settings'), hint: mac ? '⌘,' : 'Ctrl+,', go: go(() => openSettingsModal()) },
      ] as Item[]
    ).filter((c) => matchQuery(q, c.title))
    return [...agents, ...hostItems, ...commands]
  }, [sessions, hosts, q, path, onClose, onHost])
  const sel = Math.min(at, Math.max(0, items.length - 1))
  const list = useRef<HTMLDivElement>(null)
  useEffect(() => setAt(0), [q])
  useEffect(() => {
    list.current?.querySelector<HTMLElement>('[aria-selected="true"]')?.scrollIntoView({ block: 'nearest' })
  }, [sel])
  const onKey = (e: React.KeyboardEvent) => {
    if (composing(e.nativeEvent)) return // (an input method's Enter takes its word, not a row)
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      if (items.length) setAt((sel + (e.key === 'ArrowDown' ? 1 : items.length - 1)) % items.length)
    } else if (e.key === 'Enter') {
      e.preventDefault()
      items[sel]?.go()
    }
  }
  const heads: Record<Item['group'], string> = { agents: t('palette.agents'), hosts: t('palette.hosts'), commands: t('palette.commands') }
  return (
    <Dialog title={t('palette.title')} onClose={onClose} sheet={false} hideTitle className="palette">
      <label className="palette-search">
        <Command size={16} aria-hidden="true" />
        {/* (Focused by the dialog's trap, which first notes where the focus was, to give it back.) */}
        <input
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={onKey}
          placeholder={t('palette.placeholder')}
          aria-label={t('palette.placeholder')}
          role="combobox"
          aria-expanded="true"
          aria-controls="palette-list"
          aria-activedescendant={items[sel] ? 'pal-' + sel : undefined}
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
        />
        <kbd className="palette-esc">Esc</kbd>
      </label>
      <div className="palette-list" id="palette-list" role="listbox" ref={list} aria-label={t('palette.title')}>
        {!items.length && <p className="palette-none">{sessions === null || hosts === null ? t('common.loading') : t('palette.none')}</p>}
        {items.map((it, i) => (
          <div key={it.key} className="palette-group-item">
            {(i === 0 || items[i - 1].group !== it.group) && (
              <div className="palette-head" aria-hidden="true">
                {heads[it.group]}
              </div>
            )}
            <div
              id={'pal-' + i}
              role="option"
              aria-selected={i === sel}
              className="palette-row"
              onPointerMove={() => i !== sel && setAt(i)}
              onClick={() => it.go()}
            >
              <span className="palette-ic">{it.icon}</span>
              <span className="palette-text">
                <span className="palette-title">{it.title}</span>
                {it.sub && <span className="palette-sub">{it.sub}</span>}
              </span>
              {it.hint && <span className={'palette-hint' + (it.tone ? ' is-' + it.tone : '')}>{it.hint}</span>}
            </div>
          </div>
        ))}
      </div>
      <div className="palette-foot" aria-hidden="true">
        <span>
          <kbd>↑</kbd> <kbd>↓</kbd> {t('palette.move')}
        </span>
        <span>
          <kbd>↵</kbd> {t('palette.open')}
        </span>
      </div>
    </Dialog>
  )
}
