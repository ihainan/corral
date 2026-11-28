// The dock: a panel beside the thread on a wide screen (over it, from the
// right, where the thread would keep too little of its own — as the turn's
// panels are), its tabs what the agent's work is looked at in without a
// dialog over it: the Changes of its folder, its Files, its Terminal as it
// is now. Shown in the slot those panels are in, and put aside
// while one of them is open. Opened from the bar's toggle or the card's
// counts; whether it is open, its tab and its width are the person's (kept
// in this browser); what each tab shows is per conversation, kept while the
// page is.
import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { FolderOpen, GitCompareArrows, Info, Maximize2, Minimize2, SquareArrowOutUpRight, SquareTerminal, X } from 'lucide-react'
import { api } from '../api'
import { t } from '../i18n'
import { usePrefs } from '../prefs'
import { ChangesBody, useChanges, type ChangesActs, type ChangesAgainst } from './changesView'
import { PanelWindow } from './PanelWindow'
import { FilesBrowser } from './FilesDialog'
import { TextPreview } from './textPreview'
import { CardBody, type CardActs, type CardData } from './sessionCard'
import type { LineSpan } from './paths'

export type DockTab = 'info' | 'changes' | 'files' | 'terminal'
const TABS: DockTab[] = ['info', 'changes', 'files', 'terminal']
const TAB_WORD = { info: 'dock.info', changes: 'dock.changes', files: 'dock.files', terminal: 'dock.terminal' } as const

// What the page asks of the Files tab (AgentsChat's file acts, where there is
// a dock): a text file looked at (at its lines), a folder opened, a file shown
// in its folder. n: each its own, taken once.
export type FilesAsk = { kind: 'file' | 'open' | 'reveal'; path: string; lines?: LineSpan; n: number }
export type DockSink = { file: (path: string, lines?: LineSpan) => void; files: (path: string, dir: boolean) => void }

// What the Terminal tab is given: whether the pane's screen can be shown
// here (liveFor: AgentsChat's one switch for it), what a key is pressed with,
// the whole terminal.
// (letters: a letter pressed only on the screen shown — Codex's, as the composer's live panel.)
export type DockTerminal = { can: boolean; press: () => Record<string, unknown>; onFull: () => void; letters?: boolean; who?: string; onComposer?: () => void; onInput?: () => void }

// The person's: kept in this browser (none there — private, blocked: as at first).
const KEY = 'corral.dock'
// (big: expanded over the thread's column — kept too, for every agent: one setting of the browser's, as its being
// open is; doc item 39, 11-24. Read by each conversation as an agent comes in, not said live to a split's other.)
type Kept = { open: boolean; tab: DockTab; width: number; big: boolean }
export const DOCK_W = { min: 320, max: 720, at: 420 }
export function dockKept(): Kept {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) ?? 'null') as Partial<Kept> | null
    return {
      open: v?.open === true,
      tab: v?.tab && TABS.includes(v.tab) ? v.tab : 'info',
      width: typeof v?.width === 'number' ? Math.min(DOCK_W.max, Math.max(DOCK_W.min, v.width)) : DOCK_W.at,
      big: v?.big === true,
    }
  } catch {
    return { open: false, tab: 'info', width: DOCK_W.at, big: false }
  }
}
export function keepDock(k: Partial<Kept>) {
  try {
    localStorage.setItem(KEY, JSON.stringify({ ...dockKept(), ...k }))
  } catch {
    /* (not kept: as it is now, this page) */
  }
}

// Per conversation, while the page is: which of the Changes it showed; and
// the card's tap last taken (each taken once: a later remount — back from
// the Terminal tab — shows what was chosen since, not the tap again).
const against = new Map<string, ChangesAgainst>()
const tapTaken = new Map<string, number>()
// The Files tab's, per conversation: the folder it was in, the file it showed.
const filesKept = new Map<string, { dir?: string; file?: { path: string; lines?: LineSpan } }>()
const askTaken = new Map<string, number>()

// The base the card names (the remote's default branch): asked as the card
// asks, for the tab to offer it.
function useBase(sid: string, pane: string, on: boolean, given?: string) {
  const [base, setBase] = useState<string | undefined>(given)
  useEffect(() => {
    if (given) return setBase(given)
    if (!on) return
    const ctl = new AbortController()
    api<{ base?: string }>('GET', `/api/sessions/${sid}/git` + (pane ? '?pane=' + encodeURIComponent(pane) : ''), undefined, { signal: ctl.signal, timeoutMs: 20000 }).then(
      (g) => !ctl.signal.aborted && setBase(g.base),
      () => {},
    )
    return () => ctl.abort()
  }, [sid, pane, on, given])
  return base
}

export function Dock({
  of,
  ident,
  pane,
  tab: tabAsked,
  onTab,
  info,
  start,
  ask,
  onInsert,
  terminal,
  noTerminal,
  acts,
  over,
  expanded,
  canExpand,
  onExpand,
  width,
  onWidth,
  onClose,
  active,
  onSettled,
  toast,
}: {
  of: string // (the conversation: "<session> <pane>")
  pane: string // (its pane: of's, or a tmux session's conversation's own)
  ident: string // (the agent there now — its process and session: another, /resume or a new one in the pane, is shown anew, no key of the last one's screen going to it)
  tab: DockTab
  onTab: (t: DockTab) => void
  info?: { data: CardData; acts: CardActs }
  start?: { against: ChangesAgainst; base?: string; n: number } // (the card's counts tapped: that reading, n each time)
  ask?: FilesAsk // (the Files tab asked for a file or a folder)
  onInsert: (path: string) => void // (a path put in the composer, from the Files tab)
  terminal?: DockTerminal
  noTerminal?: boolean // (the conversation shows its terminal in its own place, the main view: not twice, nor two connections to one pane)
  acts?: ChangesActs
  over: boolean // (over the thread, not beside it)
  expanded: number // (over the whole of the thread's column, this wide — the side list left in sight: for a screen wider than the dock; 0: not)
  canExpand: boolean // (the thread's column wider than the dock: something to expand to)
  onExpand: (v: boolean) => void
  width: number
  onWidth: (w: number) => void
  onClose: () => void
  active: boolean
  onSettled: () => void
  toast: (s: string, bad?: boolean) => void
}) {
  usePrefs()
  const tabs = noTerminal ? TABS.filter((k) => k !== 'terminal') : TABS
  const tab: DockTab = noTerminal && tabAsked === 'terminal' ? 'info' : tabAsked
  const uid = useId() // (its tabs' and panels' ids its own: another agent's dock, kept in a tab behind, or the split's, has its own)
  const sid = of.split(' ')[0]
  const [filesShown, setFilesShown] = useState(tab === 'files')
  useEffect(() => {
    if (tab === 'files') setFilesShown(true)
  }, [tab])
  const root = useRef<HTMLElement>(null)
  // (Tabs as a tablist: the arrows between them, Home and End; the one shown the one reached by Tab.)
  const tabKeys = (e: ReactKeyboardEvent) => {
    const i = tabs.indexOf(tab)
    const to = e.key === 'ArrowRight' ? tabs[(i + 1) % tabs.length] : e.key === 'ArrowLeft' ? tabs[(i + tabs.length - 1) % tabs.length] : e.key === 'Home' ? tabs[0] : e.key === 'End' ? tabs[tabs.length - 1] : null
    if (!to) return
    e.preventDefault()
    onTab(to)
    requestAnimationFrame(() => root.current?.querySelector<HTMLElement>(`[data-tab="${to}"]`)?.focus())
  }
  // (Esc closes it only from within it — the composer's Esc stops the agent. The
  // terminal's screen takes its own keys, Esc too, before they come here.)
  const onKey = (e: ReactKeyboardEvent) => {
    if (e.key !== 'Escape' || e.defaultPrevented) return
    e.preventDefault()
    e.stopPropagation()
    // (Expanded: back beside the thread first.)
    if (expanded) onExpand(false)
    else onClose()
  }
  // Its edge, to make it wider or narrower: by a drag, or the arrows when focused.
  const drag = (e: React.PointerEvent) => {
    e.preventDefault()
    const x0 = e.clientX
    const w0 = width
    const move = (ev: PointerEvent) => onWidth(Math.min(DOCK_W.max, Math.max(DOCK_W.min, w0 + (x0 - ev.clientX))))
    const up = () => {
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', up)
    }
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', up)
  }
  const gripKeys = (e: ReactKeyboardEvent) => {
    const step = e.shiftKey ? 80 : 20
    const w = e.key === 'ArrowLeft' ? width + step : e.key === 'ArrowRight' ? width - step : e.key === 'Home' ? DOCK_W.max : e.key === 'End' ? DOCK_W.min : null
    if (w === null) return
    e.preventDefault()
    onWidth(Math.min(DOCK_W.max, Math.max(DOCK_W.min, w)))
  }
  return (
    <aside className={'convo-panel convo-tray' + (over || expanded ? ' is-over' : '') + (expanded ? ' is-expanded' : '')} style={{ width: expanded || width }} aria-label={t('dock.title')} ref={root} onKeyDown={onKey}>
      {/* (Its edge kept over the thread too: one gone over it as it widened can be made narrower again.) */}
      {!expanded && (
        <div
          className="convo-tray-grip"
          role="separator"
          aria-orientation="vertical"
          aria-label={t('dock.width')}
          aria-valuemin={DOCK_W.min}
          aria-valuemax={DOCK_W.max}
          aria-valuenow={width}
          tabIndex={0}
          onPointerDown={drag}
          onKeyDown={gripKeys}
        />
      )}
      <div className="convo-tray-head">
        <div className="convo-tray-tabs" role="tablist" aria-label={t('dock.title')} onKeyDown={tabKeys}>
          {tabs.map((k) => (
            <button
              key={k}
              type="button"
              role="tab"
              id={uid + 'tab-' + k}
              data-tab={k}
              className="convo-tray-tab"
              aria-selected={tab === k}
              aria-controls={uid + 'panel-' + k}
              tabIndex={tab === k ? 0 : -1}
              onClick={() => onTab(k)}
            >
              {k === 'info' ? <Info size={14} strokeWidth={1.75} /> : k === 'changes' ? <GitCompareArrows size={14} strokeWidth={1.75} /> : k === 'files' ? <FolderOpen size={14} strokeWidth={1.75} /> : <SquareTerminal size={14} strokeWidth={1.75} />}
              {t(TAB_WORD[k])}
            </button>
          ))}
        </div>
        {tab === 'terminal' && terminal?.can && (
          <button type="button" className="icon-btn convo-tray-full" onClick={terminal.onFull} aria-label={t('panel.full')} title={t('panel.full')}>
            <SquareArrowOutUpRight size={16} strokeWidth={1.75} />
          </button>
        )}
        {(canExpand || !!expanded) && (
          <button type="button" className="icon-btn convo-tray-expand" aria-pressed={!!expanded} onClick={() => onExpand(!expanded)} aria-label={t(expanded ? 'dock.collapse' : 'dock.expand')} title={t(expanded ? 'dock.collapse' : 'dock.expand')}>
            {expanded ? <Minimize2 size={16} strokeWidth={1.75} /> : <Maximize2 size={16} strokeWidth={1.75} />}
          </button>
        )}
        <button type="button" className="icon-btn convo-tray-close" onClick={onClose} aria-label={t('dock.close')} title={t('dock.close')}>
          <X size={16} strokeWidth={1.75} />
        </button>
      </div>
      {/* (Files kept once shown — its folder, an upload going — while another tab is: hidden, not gone.) */}
      {filesShown && (
        <div className="convo-tray-body" role="tabpanel" id={uid + 'panel-files'} aria-labelledby={uid + 'tab-files'} hidden={tab !== 'files'}>
          <DockFiles key={of + ' ' + ident} of={of} mem={of + ' ' + ident} sid={sid} pane={pane} ask={ask} onInsert={onInsert} active={active && tab === 'files'} />
        </div>
      )}
      <div className="convo-tray-body" role="tabpanel" id={tab === 'files' ? undefined : uid + 'panel-' + tab} aria-labelledby={tab === 'files' ? undefined : uid + 'tab-' + tab} hidden={tab === 'files'}>
        {tab === 'files' ? null : tab === 'info' ? (
          info ? (
            <div className="convo-tray-info">
              <div className="scard">
                <CardBody key={of + ' ' + ident} data={info.data} acts={info.acts} close={() => {}} />
              </div>
            </div>
          ) : null
        ) : tab === 'changes' ? (
          <DockChanges key={of + ' ' + ident} of={of} sid={sid} pane={pane} start={start} acts={acts} />
        ) : terminal?.can ? (
          <PanelWindow key={of + ' ' + ident} sid={sid} active={active} press={terminal.press} onSettled={onSettled} onInput={terminal.onInput} toast={toast} live={{ bare: true, onFull: terminal.onFull, letters: terminal.letters, who: terminal.who, onComposer: terminal.onComposer, onLeave: () => root.current?.querySelector<HTMLElement>('.convo-tray-tab[aria-selected="true"]')?.focus() }} />
        ) : (
          <div className="convo-tray-note">
            <p>{t('dock.noScreen')}</p>
            {terminal && (
              <button type="button" className="btn" onClick={terminal.onFull}>
                <SquareTerminal size={15} /> {t('convo.showTerminal')}
              </button>
            )}
          </div>
        )}
      </div>
    </aside>
  )
}

// The Changes tab: the view's body, its reading the one this conversation
// showed last (or the card's, tapped), a line over it to read again.
function DockChanges({ of, sid, pane, start, acts }: { of: string; sid: string; pane: string; start?: { against: ChangesAgainst; base?: string; n: number }; acts?: ChangesActs }) {
  const base = useBase(sid, pane, true, start?.base)
  const fresh = (s?: { n: number }) => !!s && tapTaken.get(of) !== s.n
  const [first] = useState(() => (fresh(start) ? start!.against : against.get(of) ?? 'work'))
  const c = useChanges(sid, pane, first)
  // (The card's counts tapped — at the first or again, the dock open already: that reading, once.)
  useEffect(() => {
    if (!start || !fresh(start)) return
    tapTaken.set(of, start.n)
    if (c.against !== start.against) c.setAgainst(start.against)
  })
  useEffect(() => void against.set(of, c.against), [of, c.against])
  return (
    <>
      <div className="convo-tray-bar">
        <span className="convo-tray-sub" title={c.root}>
          {c.sub ?? t('common.loading')}
        </span>
        <button type="button" className="link-btn" onClick={c.reload}>
          {t('preview.reload')}
        </button>
      </div>
      <ChangesBody c={c} base={base} acts={acts} className="is-docked" />
    </>
  )
}

// The Files tab: the file panel's browser, in the conversation's folder at
// first (its pane's, asked of the host — never the focused one's: none known,
// none asked, and said so), where it was after; a text file looked at shown
// in the tab (its close, or Esc, back to the folder). What the page asks
// (ask) is taken once, and gone to in the same browser — an upload going is
// not dropped for it. Kept per agent: the conversation's pane, and its agent
// there (another, /resume, starts anew — review, 11-15).
function DockFiles({ of, mem, sid, pane, ask, onInsert, active }: { of: string; mem: string; sid: string; pane: string; ask?: FilesAsk; onInsert: (path: string) => void; active: boolean }) {
  const root = useRef<HTMLDivElement>(null)
  const kept = filesKept.get(mem) ?? {}
  const fresh = !!ask && askTaken.get(of) !== ask.n
  const [view, setView] = useState(() => {
    if (fresh) askTaken.set(of, ask!.n)
    return fresh ? fromAsk(ask!, kept) : { start: kept.dir ?? null, reveal: undefined as string | undefined, file: kept.file, n: 0, asked: 0 }
  })
  // (Another ask, the tab open already: that one — a folder gone to in the same browser, a file asked again scrolled to again.)
  useEffect(() => {
    if (!ask || askTaken.get(of) === ask.n) return
    askTaken.set(of, ask.n)
    setView((v) => {
      const a = fromAsk(ask, filesKept.get(mem) ?? {})
      return ask.kind === 'file' ? { ...v, file: a.file, asked: v.asked + 1 } : { ...a, n: v.n + 1, asked: v.asked }
    })
  }, [ask, of, mem])
  useEffect(() => void filesKept.set(mem, { ...(filesKept.get(mem) ?? {}), file: view.file }), [mem, view.file])
  // (Its folder at first: the pane's, asked; not known — no pane, or the host said none: home, said so.)
  const [cwd, setCwd] = useState<string | null | undefined>(view.start || view.reveal ? null : pane ? undefined : null)
  useEffect(() => {
    if (cwd !== undefined) return
    const ctl = new AbortController()
    api<{ path?: string }>('GET', `/api/sessions/${sid}/files/cwd?pane=` + encodeURIComponent(pane), undefined, { signal: ctl.signal, timeoutMs: 12000 }).then(
      (r) => !ctl.signal.aborted && setCwd(r.path || null),
      () => !ctl.signal.aborted && setCwd(null),
    )
    return () => ctl.abort()
  }, [cwd, sid, pane])
  // (Said as it is found — the home folder listed after does not make it known.)
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const unknown = useMemo(() => cwd === null && !view.start && !view.reveal && !filesKept.get(mem)?.dir, [cwd])
  if (cwd === undefined) return <p className="convo-tray-note">{t('common.loading')}</p>
  const back = () => {
    setView((v) => ({ ...v, file: undefined }))
    // (Its focus to the folder: a second Esc there shuts the dock.)
    requestAnimationFrame(() => root.current?.querySelector<HTMLElement>('.files-list button, .crumb')?.focus())
  }
  return (
    <>
      <div className="convo-tray-files" hidden={!!view.file} ref={root}>
        {unknown && <p className="convo-tray-hint">{t('dock.noCwd')}</p>}
        <FilesBrowser
          sessionId={sid}
          start={view.start ?? cwd}
          reveal={view.reveal}
          pane={pane || null}
          go={view.n}
          active={active}
          onPath={(p) => filesKept.set(mem, { ...(filesKept.get(mem) ?? {}), dir: p })}
          onInsert={onInsert}
          onFile={(p) => setView((v) => ({ ...v, file: { path: p } }))}
        />
      </div>
      {view.file && (
        <TextPreview
          key={view.file.path + ':' + (view.file.lines?.from ?? '')}
          sid={sid}
          path={view.file.path}
          lines={view.file.lines}
          asked={view.asked}
          docked
          onClose={back}
          onReveal={() => setView((v) => ({ start: null, reveal: v.file!.path, file: undefined, n: v.n + 1, asked: v.asked }))}
        />
      )}
    </>
  )
}

function fromAsk(a: FilesAsk, kept: { dir?: string }): { start: string | null; reveal?: string; file?: { path: string; lines?: LineSpan }; n: number; asked: number } {
  if (a.kind === 'file') return { start: kept.dir ?? null, reveal: undefined, file: { path: a.path, lines: a.lines }, n: 0, asked: 0 }
  if (a.kind === 'open') return { start: a.path, reveal: undefined, file: undefined, n: 0, asked: 0 }
  return { start: null, reveal: a.path, file: undefined, n: 0, asked: 0 }
}
