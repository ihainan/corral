import type { I18nKey } from '../i18n'

// What the shortcut panel offers (long press on Ctrl, or its own key), by
// page: Ctrl combinations; the multiplexer's (herdr, by its API, or tmux with
// its default prefix); Claude Code's and Codex's. Keys are sequences as
// custom keys take them (macro.ts); slash commands are typed and sent.

export type PanelPage = 'ctrl' | 'herdr' | 'tmux' | 'claude' | 'codex'
export type HerdrAct = 'new_workspace' | 'new_tab' | 'split_right' | 'split_down' | 'zoom_toggle' | 'focus_left' | 'focus_right' | 'focus_up' | 'focus_down' | 'prev_tab' | 'next_tab' | 'close_pane'
// label: shown as it is ('' for an icon, by the herdr action); desc: an i18n key.
export type Shortcut = { id?: string; label: string; desc: I18nKey } & ({ keys: string } | { herdr: HerdrAct })

const ctrl = (c: string, desc: I18nKey): Shortcut => ({ label: '^' + c.toUpperCase(), desc, keys: 'C-' + c })
const slash = (cmd: string, desc: I18nKey): Shortcut => ({ label: '/' + cmd, desc, keys: `/${cmd} Enter` })

// Each page in groups, with a heading (an i18n key).
export type ShortcutGroup = { title: I18nKey; items: Shortcut[] }
export const shortcuts: Record<PanelPage, ShortcutGroup[]> = {
  ctrl: [
    { title: 'sc.gProgram', items: [ctrl('c', 'sc.interrupt'), ctrl('d', 'sc.eof'), ctrl('z', 'sc.suspend'), ctrl('l', 'sc.clear'), ctrl('r', 'sc.history'), ctrl('g', 'sc.cancel'), ctrl('\\', 'sc.quit')] },
    { title: 'sc.gLine', items: [ctrl('a', 'sc.lineStart'), ctrl('e', 'sc.lineEnd'), ctrl('u', 'sc.killStart'), ctrl('k', 'sc.killEnd'), ctrl('w', 'sc.killWord'), ctrl('y', 'sc.yank')] },
  ],
  herdr: [], // (by the host's bindings: herdrGroups)
  tmux: [
    {
      title: 'sc.gWindows',
      items: [
        { label: '^B c', desc: 'sc.newWindow', keys: 'C-b c' },
        { label: '^B n', desc: 'sc.nextWindow', keys: 'C-b n' },
        { label: '^B p', desc: 'sc.prevWindow', keys: 'C-b p' },
        { label: '^B w', desc: 'sc.windows', keys: 'C-b w' },
      ],
    },
    {
      title: 'sc.gPanes',
      items: [
        { label: '^B %', desc: 'sc.splitRight', keys: 'C-b %' },
        { label: '^B "', desc: 'sc.splitDown', keys: `C-b '"'` },
        { label: '^B o', desc: 'sc.nextPane', keys: 'C-b o' },
        { label: '^B z', desc: 'sc.zoom', keys: 'C-b z' },
        { label: '^B [', desc: 'sc.copyMode', keys: 'C-b [' },
        { label: '^B x', desc: 'sc.closePane', keys: 'C-b x' },
      ],
    },
  ],
  claude: [
    {
      title: 'sc.gKeys',
      items: [
        { label: '⇧Tab', desc: 'sc.mode', keys: 'BTab' },
        { label: 'Esc', desc: 'sc.stop', keys: 'Esc' },
        { label: 'Esc Esc', desc: 'sc.rewind', keys: 'Esc Esc' },
        { label: '^↵', desc: 'sc.sendNow', keys: 'C-Enter' },
        { label: '^O', desc: 'sc.expand', keys: 'C-o' },
        { label: '^B', desc: 'sc.background', keys: 'C-b' },
        { label: '^T', desc: 'sc.tasks', keys: 'C-t' },
      ],
    },
    {
      title: 'sc.gInput',
      items: [
        { label: '^G', desc: 'sc.editor', keys: 'C-g' },
        { label: '^S', desc: 'sc.stash', keys: 'C-s' },
        { label: '^R', desc: 'sc.history', keys: 'C-r' },
        { label: '^L', desc: 'sc.clearInput', keys: 'C-l' },
        { label: '^_', desc: 'sc.undo', keys: 'C-_' },
        { label: '⌥P', desc: 'sc.modelPicker', keys: 'M-p' },
        { label: '⌥T', desc: 'sc.thinking', keys: 'M-t' },
        { label: '⌥O', desc: 'sc.fast', keys: 'M-o' },
      ],
    },
    {
      title: 'sc.gCommands',
      items: [
        slash('compact', 'sc.compact'), slash('clear', 'sc.clearChat'), slash('resume', 'sc.resume'), slash('rewind', 'sc.rewindCmd'),
        slash('model', 'sc.model'), slash('effort', 'sc.effort'), slash('permissions', 'sc.permissions'), slash('context', 'sc.context'),
        slash('usage', 'sc.usage'), slash('status', 'sc.status'), slash('todos', 'sc.tasks'), slash('agents', 'sc.agents'),
        slash('mcp', 'sc.mcp'), slash('memory', 'sc.memory'), slash('init', 'sc.init'), slash('export', 'sc.export'), slash('config', 'sc.config'),
      ],
    },
  ],
  codex: [
    {
      title: 'sc.gKeys',
      items: [
        { label: '⇧Tab', desc: 'sc.mode', keys: 'BTab' },
        { label: 'Esc', desc: 'sc.stop', keys: 'Esc' },
        { label: 'Esc Esc', desc: 'sc.editLast', keys: 'Esc Esc' },
        { label: '^T', desc: 'sc.transcript', keys: 'C-t' },
      ],
    },
    {
      title: 'sc.gCommands',
      items: [slash('new', 'sc.newChat'), slash('compact', 'sc.compact'), slash('resume', 'sc.resume'), slash('model', 'sc.model'), slash('status', 'sc.status'), slash('diff', 'sc.diff'), slash('review', 'sc.review'), slash('init', 'sc.init'), slash('mcp', 'sc.mcp')],
    },
  ],
}

// The row kept at the panel's foot: a page's most used, by label (herdr's
// by what they do).
const common: Record<PanelPage, string[]> = {
  ctrl: ['^C', '^D', '^L', '^R', '^U', '^W'],
  herdr: ['workspace_picker', 'new_tab', 'next_tab', 'zoom_toggle', 'cycle_pane_next', 'goto'],
  tmux: ['^B c', '^B n', '^B w', '^B %', '^B z', '^B ['],
  claude: ['⇧Tab', 'Esc', '^O', '^T', '/compact', '/clear'],
  codex: ['⇧Tab', 'Esc', '^T', '/new', '/compact', '/diff'],
}

// herdr's bindings the panel presses, as herdr ships them (config.toml can
// change them, the prefix too: the gateway sends what the host's sets).
export type HerdrKey = 'help' | 'settings' | 'goto' | 'open_notification_target' | 'workspace_picker' | 'rename_workspace' | 'close_workspace' |
  'previous_workspace' | 'next_workspace' | 'toggle_sidebar' | 'rename_tab' | 'close_tab' | 'rename_pane' | 'edit_scrollback' | 'copy_mode' |
  'cycle_pane_next' | 'cycle_pane_previous' | 'last_pane' | 'resize_mode'
const herdrDefaults: Record<HerdrKey | 'prefix', string> = {
  prefix: 'ctrl+b', help: 'prefix+?', settings: 'prefix+s', goto: 'prefix+g', open_notification_target: 'prefix+o',
  workspace_picker: 'prefix+w', rename_workspace: 'prefix+shift+w', close_workspace: 'prefix+shift+d', previous_workspace: '',
  next_workspace: '', toggle_sidebar: 'prefix+b', rename_tab: 'prefix+shift+t', close_tab: 'prefix+shift+x', rename_pane: 'prefix+shift+p',
  edit_scrollback: 'prefix+e', copy_mode: '', cycle_pane_next: 'prefix+tab', cycle_pane_previous: 'prefix+shift+tab', last_pane: '', resize_mode: 'prefix+r',
}

// The key names herdr's config takes (as its own "config check" reads them:
// no home, end, page keys or delete), and single characters.
const keyNames: Record<string, { key: string; label: string }> = {
  tab: { key: 'Tab', label: 'Tab' }, enter: { key: 'Enter', label: '↵' }, return: { key: 'Enter', label: '↵' },
  esc: { key: 'Esc', label: 'Esc' }, escape: { key: 'Esc', label: 'Esc' }, backspace: { key: 'BSpace', label: '⌫' }, bs: { key: 'BSpace', label: '⌫' },
  up: { key: 'Up', label: '↑' }, down: { key: 'Down', label: '↓' }, left: { key: 'Left', label: '←' }, right: { key: 'Right', label: '→' },
}
for (let i = 1; i <= 12; i++) keyNames['f' + i] = { key: 'F' + i, label: 'F' + i }
const charNames: Record<string, string> = {
  space: ' ', minus: '-', plus: '+', comma: ',', period: '.', slash: '/', backslash: '\\', backtick: '`', quote: "'",
  double_quote: '"', 'double-quote': '"', semicolon: ';', colon: ':', percent: '%', ampersand: '&',
}

// One chord of a herdr binding ("ctrl+b", "shift+t", "T", "?") as keys the
// way macro.ts takes them, and as shown; null for one that cannot be sent
// as herdr reads it: shift on a symbol (what it types depends on the
// keyboard), or one whose bytes are another key's (Ctrl+I is Tab, Ctrl+Tab
// is Tab, Shift+Enter would go as Alt+Enter).
function herdrChord(b: string): { keys: string; label: string } | null {
  const parts = b.split('+')
  let name = parts.pop() ?? ''
  if (name === '' && parts.length && parts[parts.length - 1] === '') (parts.pop(), (name = '+')) // ("ctrl++")
  let ctrl = false, alt = false, shift = false
  for (const m of parts.map((x) => x.toLowerCase())) {
    if (m === 'ctrl' || m === 'control') ctrl = true
    else if (m === 'alt' || m === 'meta' || m === 'option') alt = true
    else if (m === 'shift') shift = true
    else return null
  }
  // (herdr reads "T" as Shift+t.)
  if (/^[A-Z]$/.test(name)) (shift = true), (name = name.toLowerCase())
  const mods = (ctrl ? 'C-' : '') + (alt ? 'M-' : '')
  const shown = (ctrl ? '^' : '') + (alt ? '⌥' : '')
  const named = keyNames[name.toLowerCase()]
  if (named) {
    if (named.key === 'Tab') return ctrl || alt ? null : shift ? { keys: 'BTab', label: '⇧Tab' } : { keys: 'Tab', label: 'Tab' }
    if (named.key === 'Enter' || named.key === 'Esc' || named.key === 'BSpace') return ctrl || alt || shift ? null : { keys: named.key, label: named.label }
    return { keys: mods + (shift ? 'S-' : '') + named.key, label: shown + (shift ? '⇧' : '') + named.label }
  }
  const ch = charNames[name.toLowerCase()] ?? name
  if ([...ch].length !== 1 || /[\x00-\x20\x7f]/.test(ch)) return ch === ' ' && !ctrl && !alt && !shift ? { keys: '\\ ', label: '␣' } : null
  if (shift && !/^[a-z]$/.test(ch)) return null
  const c = shift ? ch.toUpperCase() : ch
  // Ctrl: a letter, not one whose control character is another key's (Tab,
  // Enter, Backspace, newline), nor with Shift; Alt: not the characters
  // macro.ts reads as its own (quotes, a backslash).
  if (ctrl && (!/^[a-z]$/.test(ch) || shift || 'ihjm'.includes(ch))) return null
  if (mods && /["'\\]/.test(c)) return null
  // (Text, a backslash before anything but a letter or digit: see macro.ts.)
  return { keys: mods ? mods + c : /^[A-Za-z0-9]$/.test(c) ? c : '\\' + c, label: shown + (shift ? '⇧' : '') + (ctrl ? c.toUpperCase() : c) }
}

// A binding as keys to press and its label; null when unbound (or one
// herdr would read as a range, as "prefix+1..9").
export function herdrBinding(b: string, prefix: string): { keys: string; label: string } | null {
  if (!b || b.includes('..')) return null
  if (b.startsWith('prefix+') && !prefix) return null
  const steps = b.startsWith('prefix+') ? [prefix, b.slice(7)] : [b]
  const out = steps.map(herdrChord)
  if (out.some((x) => !x)) return null
  return { keys: out.map((x) => x!.keys).join(' '), label: out.map((x) => x!.label).join(' ') }
}

// The herdr page, by this host's bindings: its own actions through its API
// (the same whatever the keys), the rest as herdr's keys.
export function herdrGroups(set?: Record<string, string>, unknown?: boolean): ShortcutGroup[] {
  // (Bindings not known, as the host's config could not be read: only the API's actions.)
  const bound = (k: HerdrKey | 'prefix') => (unknown ? '' : set && k in set ? set[k] : herdrDefaults[k])
  const prefix = bound('prefix') // ("": not known, or none: what needs it is left out)
  const key = (k: HerdrKey, desc: I18nKey): Shortcut[] => {
    const b = herdrBinding(bound(k), prefix)
    return b ? [{ id: k, label: b.label, desc, keys: b.keys }] : []
  }
  const act = (a: HerdrAct, desc: I18nKey): Shortcut => ({ id: a, label: '', desc, herdr: a })
  const groups: ShortcutGroup[] = [
    {
      title: 'sc.gWorkspaces',
      items: [
        ...key('workspace_picker', 'sc.workspaces'), ...key('goto', 'sc.goto'), act('new_workspace', 'sc.newWorkspace'),
        ...key('previous_workspace', 'sc.prevWorkspace'), ...key('next_workspace', 'sc.nextWorkspace'),
        ...key('rename_workspace', 'sc.renameWorkspace'), ...key('close_workspace', 'sc.closeWorkspace'), ...key('toggle_sidebar', 'sc.sidebar'),
      ],
    },
    {
      title: 'sc.gTabs',
      items: [act('new_tab', 'sc.newTab'), act('prev_tab', 'sc.prevTab'), act('next_tab', 'sc.nextTab'), ...key('rename_tab', 'sc.renameTab'), ...key('close_tab', 'sc.closeTab')],
    },
    {
      title: 'sc.gPanes',
      items: [
        act('split_right', 'sc.splitRight'), act('split_down', 'sc.splitDown'), act('zoom_toggle', 'sc.zoom'),
        act('focus_left', 'sc.paneLeft'), act('focus_right', 'sc.paneRight'), act('focus_up', 'sc.paneUp'), act('focus_down', 'sc.paneDown'),
        ...key('cycle_pane_next', 'sc.nextPane'), ...key('cycle_pane_previous', 'sc.prevPane'), ...key('last_pane', 'sc.lastPane'),
        ...key('rename_pane', 'sc.renamePane'), ...key('resize_mode', 'sc.resizePanes'), act('close_pane', 'sc.closePane'),
      ],
    },
    {
      title: 'sc.gMore',
      items: [...key('help', 'sc.herdrHelp'), ...key('settings', 'sc.settings'), ...key('open_notification_target', 'sc.notifyTarget'), ...key('edit_scrollback', 'sc.editScrollback'), ...key('copy_mode', 'sc.copyMode')],
    },
  ]
  return groups.filter((g) => g.items.length)
}

// A page's groups, and its foot row.
export function pageGroups(page: PanelPage, herdrKeys?: Record<string, string>, keysUnknown?: boolean): ShortcutGroup[] {
  return page === 'herdr' ? herdrGroups(herdrKeys, keysUnknown) : shortcuts[page]
}
export function pageCommon(groups: ShortcutGroup[], page: PanelPage): Shortcut[] {
  const all = groups.flatMap((g) => g.items)
  return common[page].flatMap((c) => all.filter((s) => (s.id ?? s.label) === c).slice(0, 1))
}

export const pageTitles: Record<PanelPage, string> = { ctrl: 'Ctrl', herdr: 'herdr', tmux: 'tmux', claude: 'Claude Code', codex: 'Codex' }

// The pages for a session, and the one to open on: the agent in herdr's
// focused pane, else the multiplexer, else the page used last.
export function panelPages(persistence: string | undefined, focusedAgent: string | undefined, last: string | null): { pages: PanelPage[]; open: PanelPage } {
  const mux: PanelPage[] = persistence === 'herdr' ? ['herdr'] : persistence === 'tmux' ? ['tmux'] : []
  const pages: PanelPage[] = ['ctrl', ...mux, 'claude', 'codex']
  const agent = (focusedAgent ?? '').toLowerCase()
  const open: PanelPage = agent.includes('claude') ? 'claude' : agent.includes('codex') ? 'codex' : mux[0] ?? (pages.includes(last as PanelPage) ? (last as PanelPage) : 'ctrl')
  return { pages, open }
}
