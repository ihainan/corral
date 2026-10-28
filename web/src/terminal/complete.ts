// Completion in the composer, as the agents' own input boxes offer it: "@"
// and a path (files under the folder the terminal is in), "/" at the start of
// the message (the agents' commands, and those found on the host) and "$" (a
// Codex skill).

export type Agent = 'claude' | 'codex' | 'opencode'

export interface Completions {
  cwd: string
  files: string[]
  commands: { agent: Agent; name: string }[]
  more: boolean // the folder has more files than listed
}

// The agents' built-in commands, as their own input boxes list them
// (Claude Code 2.1, Codex 0.16x, OpenCode 1.19; a command a version lacks is
// refused there).
const builtin: Record<Agent, string[]> = {
  claude: [
    '/add-dir', '/advisor', '/agents', '/auto-mode-setup', '/autocompact', '/batch', '/branch', '/btw', '/bug',
    '/cd', '/clear', '/color', '/compact', '/config', '/context', '/copy', '/cost', '/debug', '/diff', '/doctor',
    '/effort', '/exit', '/export', '/fast', '/feedback', '/fewer-permission-prompts', '/focus', '/fork', '/goal',
    '/help', '/hooks', '/ide', '/import', '/init', '/insights', '/install-github-app', '/keybindings',
    '/list-agents', '/login', '/logout', '/loop', '/mcp', '/memory', '/mobile', '/model', '/output-style',
    '/permissions', '/plan', '/plugin', '/powerup', '/pr-comments', '/radio', '/recap', '/release-notes', '/rename',
    '/resume', '/review', '/rewind', '/run', '/sandbox', '/security-review', '/skills', '/status', '/statusline',
    '/subtask', '/tasks', '/terminal-setup', '/theme', '/todos', '/tui', '/update-config', '/usage', '/verify',
    '/vim', '/workflows',
  ],
  codex: [
    '/agents', '/approve', '/archive', '/btw', '/cd', '/clear', '/compact', '/copy', '/daemon', '/delete', '/diff',
    '/exit', '/experimental', '/export', '/feedback', '/fork', '/goal', '/hooks', '/ide', '/import', '/init',
    '/keymap', '/logout', '/mcp', '/memories', '/mention', '/model', '/new', '/permissions', '/pets', '/plan',
    '/plugins', '/ps', '/pwd', '/quit', '/raw', '/recap', '/rename', '/resume', '/review', '/side', '/skills',
    '/status', '/statusline', '/stop', '/subagents', '/theme', '/title', '/tui', '/vim', '/voice', '/warnings',
    '/worktree',
  ],
  opencode: [
    '/agents', '/compact', '/connect', '/copy', '/debug', '/diff', '/editor', '/exit', '/export', '/fork', '/help',
    '/init', '/mcps', '/models', '/move', '/new', '/redo', '/rename', '/review', '/sessions', '/share', '/skills',
    '/status', '/themes', '/thinking', '/timeline', '/timestamps', '/undo', '/unshare', '/update',
  ],
}

// What is being completed: the word before the caret, when it starts with
// "@" (anywhere), "/" (at the start of the message) or "$". start..end is
// the range it replaces.
export interface Token {
  kind: '@' | '/' | '$'
  query: string
  start: number
  end: number
}

export function tokenAt(text: string, caret: number): Token | null {
  let i = caret
  while (i > 0 && !/\s/.test(text[i - 1])) i--
  const word = text.slice(i, caret)
  if (word.length === 0) return null
  const kind = word[0]
  // (Only up to the caret: a word continuing after it is not offered for.)
  if (caret < text.length && !/\s/.test(text[caret])) return null
  if (kind === '@') return { kind, query: word.slice(1), start: i, end: caret }
  if (kind === '/' && text.slice(0, i).trim() === '' && !word.slice(1).includes('/')) return { kind, query: word.slice(1), start: i, end: caret }
  if (kind === '$' && /^\$[A-Za-z0-9._-]*$/.test(word)) return { kind, query: word.slice(1), start: i, end: caret }
  return null
}

// score: how well a candidate matches the query (higher is better), or -1.
// Letters must come in order; matches at the start of the name, of a path
// segment or of a word count most; shorter candidates win ties.
export function score(cand: string, query: string): number {
  if (!query) return 1000 - cand.length
  const c = cand.toLowerCase()
  const q = query.toLowerCase()
  const base = c.slice(c.lastIndexOf('/', c.length - 2) + 1)
  if (base.startsWith(q)) return 3000 - cand.length
  const at = c.indexOf(q)
  if (at >= 0) return 2000 - at - cand.length / 10
  let j = 0
  let s = 0
  let prev = -2
  for (let i = 0; i < c.length && j < q.length; i++) {
    if (c[i] !== q[j]) continue
    const boundary = i === 0 || '/._- '.includes(c[i - 1])
    s += boundary ? 8 : i === prev + 1 ? 4 : 1
    prev = i
    j++
  }
  return j === q.length ? s * 10 - cand.length / 10 : -1
}

export interface Suggestion {
  insert: string // what replaces the token (followed by a space)
  label: string
  hint?: string // the agent, for commands
}

const MAX_SHOWN = 8

// suggest lists what to offer for a token. agent: the agent known to be in
// front (herdr's focused pane), else Claude Code's and Codex's commands are
// offered.
export function suggest(tok: Token, data: Completions | null, agent: Agent | null): Suggestion[] {
  // Scattered letters only count when nothing has the query as it is typed.
  const rank = <T>(items: T[], key: (x: T) => string) => {
    const q = tok.query.toLowerCase()
    const whole = q ? items.filter((x) => key(x).toLowerCase().includes(q)) : items
    return (whole.length > 0 ? whole : items)
      .map((x) => ({ x, s: score(key(x), tok.query) }))
      .filter((e) => e.s >= 0)
      .sort((a, b) => b.s - a.s)
      .slice(0, MAX_SHOWN)
      .map((e) => e.x)
  }
  if (tok.kind === '@') {
    if (!data) return []
    // Folders too, as the agents offer them ("src/"), but not one with a
    // space: quoted, it could not be completed further. (Bounded: the
    // listing comes from the host.)
    const dirs = new Set<string>()
    for (const f of data.files) {
      if (f.length > 512 || dirs.size >= 20000) continue
      for (let i = f.indexOf('/'); i >= 0; i = f.indexOf('/', i + 1)) {
        const d = f.slice(0, i + 1)
        if (!/[\s"]/.test(d)) dirs.add(d)
      }
    }
    return rank([...dirs, ...data.files], (p) => p).map((p) => ({ insert: '@' + quoteAt(p), label: p }))
  }
  const agents: Agent[] = agent ? [agent] : ['claude', 'codex']
  const cmds: { agent: Agent; name: string }[] = []
  for (const a of agents) for (const n of builtin[a]) cmds.push({ agent: a, name: n })
  for (const c of data?.commands ?? []) if (agents.includes(c.agent)) cmds.push(c)
  const want = cmds.filter((c) => c.name[0] === tok.kind)
  // The same name for both agents is offered once, named only when it is
  // one agent's.
  const owners = new Map<string, Set<Agent>>()
  for (const c of want) owners.set(c.name, (owners.get(c.name) ?? new Set<Agent>()).add(c.agent))
  const hint = (name: string) => {
    const o = owners.get(name)!
    return agent || o.size > 1 ? undefined : o.has('claude') ? 'Claude Code' : 'Codex'
  }
  return rank([...owners.keys()], (n) => n.slice(1)).map((n) => ({ insert: n, label: n, hint: hint(n) }))
}

// A path after "@" as the agents read it: quoted when it has spaces.
function quoteAt(p: string): string {
  return /[\s"]/.test(p) ? '"' + p.replace(/"/g, '\\"') + '"' : p
}

// apply puts a suggestion in place of the token; it returns the new text and
// where the caret goes (after the space that follows).
export function apply(text: string, tok: Token, s: Suggestion): { text: string; caret: number } {
  const ins = s.insert.endsWith('/') ? s.insert : s.insert + ' '
  return { text: text.slice(0, tok.start) + ins + text.slice(tok.end), caret: tok.start + ins.length }
}
