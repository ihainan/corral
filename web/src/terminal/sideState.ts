import type { AgentStatus } from './agents'
import type { RowTask } from './agentOrder'

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
  cmd?: string
  out?: string
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
  for (const r of convoRunning ?? []) {
    if (!r.id) continue
    const parsed = r.ts ? Date.parse(r.ts) : NaN
    const since = Number.isFinite(parsed) && parsed > 0 ? parsed : undefined
    if (r.kind === 'agent') {
      const hit = agents.find((x) => x.id === r.id)
      if (hit) {
        if (!hit.name && r.what) hit.name = r.what
        if (!hit.since && since) hit.since = since
        hit.live = true
      } else {
        agents.push({ kind: 'agent', id: r.id, name: r.what || '', since, live: true })
      }
    } else {
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
