import type { I18nKey } from '../i18n'

// What the shortcut panel offers (long press on Ctrl, or its own key), by
// page: Ctrl combinations; the multiplexer's (herdr, by its API, or tmux with
// its default prefix); Claude Code's and Codex's. Keys are sequences as
// custom keys take them (macro.ts); slash commands are typed and sent.

export type PanelPage = 'ctrl' | 'herdr' | 'tmux' | 'claude' | 'codex'
export type HerdrAct = 'new_tab' | 'split_right' | 'split_down' | 'zoom_toggle' | 'focus_left' | 'focus_right' | 'focus_up' | 'focus_down' | 'prev_tab' | 'next_tab'
// label: shown as it is ('' for an icon, by the herdr action); desc: an i18n key.
export type Shortcut = { label: string; desc: I18nKey } & ({ keys: string } | { herdr: HerdrAct })

const ctrl = (c: string, desc: I18nKey): Shortcut => ({ label: '^' + c.toUpperCase(), desc, keys: 'C-' + c })
const slash = (cmd: string, desc: I18nKey): Shortcut => ({ label: '/' + cmd, desc, keys: `/${cmd} Enter` })

// Each page in groups, with a heading (an i18n key).
export type ShortcutGroup = { title: I18nKey; items: Shortcut[] }
export const shortcuts: Record<PanelPage, ShortcutGroup[]> = {
  ctrl: [
    { title: 'sc.gProgram', items: [ctrl('c', 'sc.interrupt'), ctrl('d', 'sc.eof'), ctrl('z', 'sc.suspend'), ctrl('l', 'sc.clear'), ctrl('r', 'sc.history'), ctrl('g', 'sc.cancel')] },
    { title: 'sc.gLine', items: [ctrl('a', 'sc.lineStart'), ctrl('e', 'sc.lineEnd'), ctrl('u', 'sc.killStart'), ctrl('k', 'sc.killEnd'), ctrl('w', 'sc.killWord'), ctrl('y', 'sc.yank')] },
  ],
  herdr: [
    {
      title: 'sc.gTabs',
      items: [
        { label: '', desc: 'sc.newTab', herdr: 'new_tab' },
        { label: '', desc: 'sc.splitRight', herdr: 'split_right' },
        { label: '', desc: 'sc.splitDown', herdr: 'split_down' },
        { label: '', desc: 'sc.zoom', herdr: 'zoom_toggle' },
        { label: '', desc: 'sc.prevTab', herdr: 'prev_tab' },
        { label: '', desc: 'sc.nextTab', herdr: 'next_tab' },
      ],
    },
    {
      title: 'sc.gPanes',
      items: [
        { label: '', desc: 'sc.paneLeft', herdr: 'focus_left' },
        { label: '', desc: 'sc.paneRight', herdr: 'focus_right' },
        { label: '', desc: 'sc.paneUp', herdr: 'focus_up' },
        { label: '', desc: 'sc.paneDown', herdr: 'focus_down' },
      ],
    },
  ],
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
      ],
    },
    {
      title: 'sc.gCommands',
      items: [slash('compact', 'sc.compact'), slash('clear', 'sc.clearChat'), slash('resume', 'sc.resume'), slash('model', 'sc.model'), slash('context', 'sc.context'), slash('usage', 'sc.usage')],
    },
  ],
  codex: [
    { title: 'sc.gKeys', items: [{ label: 'Esc', desc: 'sc.stop', keys: 'Esc' }] },
    {
      title: 'sc.gCommands',
      items: [slash('new', 'sc.newChat'), slash('compact', 'sc.compact'), slash('resume', 'sc.resume'), slash('model', 'sc.model'), slash('status', 'sc.status'), slash('diff', 'sc.diff'), slash('review', 'sc.review')],
    },
  ],
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
