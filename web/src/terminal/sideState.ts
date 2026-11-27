import type { AgentStatus } from './agents'
import type { BgAgentLike, RowCmds, RowTask } from './agentOrder'

// Shared reconciliation rules between herdr's periodic summary and the live
// conversation stream so the sidebar and conversation view agree on status,
// timestamps, and background tasks.

export interface ReconcileStatusInput {
  herdrStatus: AgentStatus // row.status (after withBg: done/idle + bg='running' -> 'working')
  herdrRawStatus: AgentStatus // row.a.status (raw herdr status before withBg)
  herdrSince?: number
  herdrLost?: boolean
  bg?: 'running' | 'stale'
  bgChecking?: boolean // row.a.bg === 'checking'
  bgSince?: number
  convoLive?: boolean // !convo.failing && bound to this pane
  convoAgent?: string // 'claude' | 'codex' | 'opencode' | 'agy' | 'devin'
  convoStatus?: 'busy' | 'idle' | 'waiting'
  convoWaitingFor?: string
  convoWorkingSince?: number // latest non-mid user turn ts or convo.changedAt
}

export interface ReconcileStatusResult {
  sideStatus: AgentStatus
  viewStatus: AgentStatus
  bg?: 'running' | 'stale'
  since?: number
  dialog: boolean
}

export interface RunningTaskLike {
  id: string
  what: string
  kind: string // 'cmd' | 'agent'
  ts?: string
  note?: string
  cmd?: string
  out?: string
  agent?: string
  task?: string
}

export interface EntryLike {
  role: string
  mid?: boolean
  gone?: boolean
  ts?: string
}

const answerableWaitingFor = (w?: string) =>
  w === 'permission prompt' || w === 'input needed' || w === 'sandbox request' || w === 'worker request'

// reconcileAgentStatus merges herdr's summary status with the live conversation's
// process/log status into a single source of truth for both the conversation
// header and the sidebar row:
// - When herdr is lost, keep 'unknown' (grouping falls back to lastStatus).
// - Answerable waiting prompts on screen ('permission prompt', 'input needed', ...)
//   always surface as 'blocked' even if herdr reports 'idle' or 'bg'.
// - If herdr says 'idle' or 'done' (or background-only) while the live
//   conversation process/log reports 'busy', upgrade BOTH views to 'working'
//   using the conversation's turn-start timestamp rather than herdr's stale idle time.
// - Never downgrade herdr's 'working' or 'blocked' to 'idle' based on the log
//   (a turn just started or compacting may not have flushed to the log yet).
export function reconcileAgentStatus(input: ReconcileStatusInput): ReconcileStatusResult {
  if (input.herdrLost) {
    return { sideStatus: 'unknown', viewStatus: 'unknown', bg: undefined, since: input.herdrSince, dialog: false }
  }
  const ended = (input.bg === 'running' || input.bgChecking) && (input.herdrRawStatus === 'done' || input.herdrRawStatus === 'idle')
  const herdrView: AgentStatus = ended && input.bg !== 'stale' ? input.herdrRawStatus : input.herdrStatus
  const herdrSide: AgentStatus = input.herdrStatus
  const defaultSince = input.bg ? input.bgSince : input.herdrSince

  if (!input.convoLive) {
    return { sideStatus: herdrSide, viewStatus: herdrView, bg: input.bg, since: defaultSince, dialog: false }
  }

  const ownCC = (input.convoAgent ?? 'claude') === 'claude'
  const ownAgy = input.convoAgent === 'agy'
  const asks = (ownCC || ownAgy) && input.convoStatus === 'waiting' && answerableWaitingFor(input.convoWaitingFor)

  if (asks && herdrSide !== 'unknown') {
    const since = input.herdrRawStatus === 'blocked' ? input.herdrSince : (input.convoWorkingSince ?? input.herdrSince)
    return { sideStatus: 'blocked', viewStatus: 'blocked', bg: input.bg, since, dialog: false }
  }

  if (input.convoStatus === 'busy' && herdrSide !== 'blocked' && herdrSide !== 'unknown') {
    const herdrWorking = input.herdrRawStatus === 'working'
    const since = herdrWorking
      ? (input.herdrSince ?? input.convoWorkingSince)
      : input.convoWorkingSince && (!input.herdrSince || input.convoWorkingSince > input.herdrSince)
        ? input.convoWorkingSince
        : (input.convoWorkingSince ?? input.herdrSince)
    return { sideStatus: 'working', viewStatus: 'working', bg: undefined, since, dialog: false }
  }

  const dialog = ownCC && herdrView !== 'blocked' && input.convoStatus === 'waiting' && !answerableWaitingFor(input.convoWaitingFor)
  return { sideStatus: herdrSide, viewStatus: herdrView, bg: input.bg, since: defaultSince, dialog }
}

const normCmd = (s?: string) => (s ?? '').replace(/\s+/g, ' ').trim()

const isSyntheticHostCmd = (id: string) => /^(?:monitor|codex|agy)-\d+$/.test(id)

function sameCmdText(a?: string, b?: string): boolean {
  const x = normCmd(a)
  const y = normCmd(b)
  if (!x || !y) return false
  if (x === y) return true
  const short = x.length <= y.length ? x : y
  const long = x.length <= y.length ? y : x
  return short.length >= 8 && long.startsWith(short)
}

// mergeRowTasks combines herdr's periodic host-scan tasks with the live
// conversation log's running tasks (subagents first, then commands).
// This eliminates the up-to-60s gap where the conversation footer already
// shows a background subagent or command that cmdProber has not scanned yet,
// and also covers resumed/backgrounded subagents and non-Claude/tmux agents.
export function mergeRowTasks(
  herdrTasks: RowTask[] | undefined,
  convoRunning: RunningTaskLike[] | undefined,
  opts?: { blocked?: boolean; lost?: boolean },
): RowTask[] | undefined {
  if (opts?.lost || opts?.blocked) return undefined
  const agents: RowTask[] = []
  const cmds: RowTask[] = []
  for (const x of herdrTasks ?? []) {
    if (x.kind === 'agent') agents.push({ ...x })
    else cmds.push({ ...x })
  }
  // Track background command task IDs already promoted to CLI subagents so a
  // raw cmd entry from the conversation log never duplicates a promoted CLI row.
  const promotedTasks = new Set<string>()
  for (const a of agents) {
    if (a.agent && a.task) promotedTasks.add(a.task)
  }
  for (const r of convoRunning ?? []) {
    if (r.kind === 'agent' && r.agent && r.task) promotedTasks.add(r.task)
  }
  // Drop any plain cmd from herdrTasks whose task is now promoted by convoRunning.
  for (let i = cmds.length - 1; i >= 0; i--) {
    if (promotedTasks.has(cmds[i].id)) cmds.splice(i, 1)
  }
  for (const r of convoRunning ?? []) {
    if (!r.id) continue
    const parsed = r.ts ? Date.parse(r.ts) : NaN
    const since = Number.isFinite(parsed) && parsed > 0 ? parsed : undefined
    if (r.kind === 'agent') {
      const hit = agents.find((x) => x.id === r.id && (x.agent ?? '') === (r.agent ?? '')) ?? agents.find((x) => x.id === r.id)
      if (hit) {
        if (!hit.name && r.what) hit.name = r.what
        if (!hit.since && since) hit.since = since
        if (!hit.agent && r.agent) hit.agent = r.agent
        if (!hit.task && r.task) hit.task = r.task
        if (!hit.cmd && r.cmd) hit.cmd = r.cmd
        hit.live = true
      } else {
        agents.push({
          kind: 'agent',
          id: r.id,
          name: r.what || r.cmd || '',
          cmd: r.cmd || undefined,
          since,
          live: true,
          agent: r.agent || undefined,
          task: r.task || undefined,
        })
      }
    } else {
      if (promotedTasks.has(r.id)) continue
      const hit =
        cmds.find((x) => x.id === r.id) ??
        cmds.find((x) => isSyntheticHostCmd(x.id) && (sameCmdText(x.cmd || x.name, r.cmd || r.what) || sameCmdText(x.name, r.what)))
      if (hit) {
        if (isSyntheticHostCmd(hit.id)) hit.id = r.id
        if (!hit.name && (r.what || r.cmd)) hit.name = r.what || r.cmd || ''
        if (!hit.cmd && r.cmd) hit.cmd = r.cmd
        if (!hit.out && r.out) hit.out = r.out
        if (!hit.since && since) hit.since = since
      } else {
        cmds.push({
          kind: 'cmd',
          id: r.id,
          name: r.what || r.cmd || '',
          cmd: r.cmd || undefined,
          since,
          out: r.out || undefined,
        })
      }
    }
  }
  const out = [...agents, ...cmds]
  return out.length ? out : undefined
}

// conversationRunningItems builds the unified list of running background items
// for the conversation page's "background tasks" bar and session card:
// - Upgrades/suppresses any plain cmd in convoRunning whose task ID is owned by
//   a promoted CLI subagent in agentTasks.
// - Appends promoted CLI subagents from agentTasks that are not already in
//   convoRunning (e.g. when the parent agent is done/idle and its log moved on).
// - Keeps subagents (including CLI subagents) before plain commands.
export function conversationRunningItems(
  convoRunning: RunningTaskLike[] | undefined,
  agentTasks: RowTask[] | undefined,
): RunningTaskLike[] {
  const cliFromTasks = (agentTasks ?? []).filter((x) => x.kind === 'agent' && !!x.agent)
  const promotedByTask = new Map<string, RowTask[]>()
  for (const x of cliFromTasks) {
    if (!x.task) continue
    const list = promotedByTask.get(x.task) ?? []
    list.push(x)
    promotedByTask.set(x.task, list)
  }
  const seenAgent = new Set<string>()
  const agents: RunningTaskLike[] = []
  const cmds: RunningTaskLike[] = []
  for (const r of convoRunning ?? []) {
    if (!r.id) continue
    if (r.kind === 'agent') {
      seenAgent.add(r.id)
      agents.push(r)
      continue
    }
    const promoted = promotedByTask.get(r.id)
    if (promoted && promoted.length > 0) {
      for (const x of promoted) {
        if (seenAgent.has(x.id)) continue
        seenAgent.add(x.id)
        agents.push({
          id: x.id,
          kind: 'agent',
          what: r.what || x.name || r.cmd || '',
          ts: r.ts || (x.since ? new Date(x.since).toISOString() : undefined),
          cmd: r.cmd || x.cmd,
          out: r.out || x.out,
          agent: x.agent,
          task: r.id,
        })
      }
      continue
    }
    cmds.push(r)
  }
  for (const x of cliFromTasks) {
    if (seenAgent.has(x.id)) continue
    seenAgent.add(x.id)
    agents.push({
      id: x.id,
      kind: 'agent',
      what: x.name || x.cmd || '',
      ts: x.since ? new Date(x.since).toISOString() : undefined,
      cmd: x.cmd,
      out: x.out,
      agent: x.agent,
      task: x.task,
    })
  }
  return [...agents, ...cmds]
}

// filterHostCmds removes any host-scanned background command already promoted
// to a CLI subagent in runningItems (by exact task ID or synthetic command text
// match), decrementing cmds.n accordingly so header status counts and SessionCard
// never double-count or show a promoted CLI task as a plain command.
export function filterHostCmds(cmds: RowCmds | undefined, runningItems: RunningTaskLike[]): RowCmds | undefined {
  if (!cmds) return undefined
  const cliRunning = runningItems.filter((r) => r.kind === 'agent' && !!r.agent)
  if (!cliRunning.length) return cmds
  const ownedTasks = new Set<string>()
  for (const r of cliRunning) {
    if (r.task) ownedTasks.add(r.task)
  }
  const kept: RowCmds['list'] = []
  let removed = 0
  for (const c of cmds.list) {
    const byTask = !!c.task && ownedTasks.has(c.task)
    const bySynth =
      !byTask &&
      !!c.task &&
      isSyntheticHostCmd(c.task) &&
      cliRunning.some((r) => sameCmdText(c.title || c.desc, r.cmd || r.what) || sameCmdText(c.desc || c.title, r.what))
    if (byTask || bySynth) {
      removed++
      continue
    }
    kept.push(c)
  }
  if (removed === 0) return cmds
  const n = Math.max(0, cmds.n - removed)
  return n > 0 || kept.length > 0 ? { n: Math.max(n, kept.length), list: kept } : undefined
}

// Cached tmux background state (both plain commands and promoted CLI subagents)
// so switching away from a tmux agent preserves its sidebar CLI sub-rows and
// Working-group placement until the cache expires or a fresh read clears it.
export interface TmuxBgCacheEntry {
  cmds?: RowCmds
  agents?: BgAgentLike[]
  at: number
}

export function keepTmuxBg(
  cache: Map<string, TmuxBgCacheEntry>,
  key: string,
  cmds: RowCmds | undefined,
  agents: BgAgentLike[] | undefined,
  at: number,
): void {
  const hasAgents = !!(agents && agents.length)
  if (cmds || hasAgents) cache.set(key, { cmds, agents: hasAgents ? agents : undefined, at })
  else cache.delete(key)
}

export function readTmuxBg(
  cache: Map<string, TmuxBgCacheEntry>,
  key: string,
  now: number,
  freshMs: number,
): { cmds?: RowCmds; agents?: BgAgentLike[] } {
  const v = cache.get(key)
  if (!v || now - v.at >= freshMs) return {}
  return { cmds: v.cmds, agents: v.agents }
}

// subagentRequestUrl builds the read-only subagent conversation URL, carrying
// optional cli ('agy' | 'claude' | 'codex') and cli_parent (for native subagents
// spawned inside a background CLI session).
export function subagentRequestUrl(
  sessionId: string,
  pane: string,
  agent: string,
  gen: number,
  rev: number,
  older: boolean,
  cli?: string,
  cliParent?: string,
): string {
  const q = new URLSearchParams({ pane, agent, gen: String(gen), rev: String(rev), older: older ? '1' : '0' })
  if (cli) q.set('cli', cli)
  if (cli && cliParent) q.set('cli_parent', cliParent)
  return `/api/sessions/${sessionId}/conversation/subagent?` + q.toString()
}

// shouldContinueSubPoll reports whether useSubConvo should schedule another poll:
// depth-1 subagents poll only while live; nested subagents poll while their log grew recently.
export function shouldContinueSubPoll(live: boolean, nested: boolean, grewAt: number, now: number, growingMs = 30000): boolean {
  return live || (nested && now - grewAt < growingMs)
}

// convoLastUserMs returns the unix-ms timestamp of the latest non-mid, non-gone
// user message in the conversation (matching the conversation header's timer).
export function convoLastUserMs(entries: EntryLike[]): number | undefined {
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i]
    if (e.role === 'user' && !e.mid && !e.gone && e.ts) {
      const t = Date.parse(e.ts)
      if (Number.isFinite(t) && t > 0) return t
    }
  }
  return undefined
}

// stampAgeMs computes a non-negative age in ms for sidebar timestamps.
// AgentList's `now` ticks every 30s and server clocks can sit a few seconds
// ahead of the browser, so a recent `since` within maxFutureSkewMs clamps to 0
// ("now") instead of rendering blank; bogus far-future timestamps still return null.
export function stampAgeMs(since: number | undefined, now: number, maxFutureSkewMs = 60_000): number | null {
  if (!since || since <= 0) return null
  const age = now - since
  if (age < -maxFutureSkewMs) return null
  return Math.max(0, age)
}
