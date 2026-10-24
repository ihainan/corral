import { getLang } from '../i18n'
import type { ConvoEntry, ConvoTool } from './conversation'

// What an agent asks to be allowed (a permission prompt on its screen) is
// the call it last made that has not come back: its log has the call before
// it runs. The screen is what is answered; the log only says more of it
// (the whole command, the edit), and only when the screen is seen to be of
// the same call.

const asksNothing = new Set(['TaskCreate', 'TaskUpdate', 'TodoWrite', 'AskUserQuestion'])

// The calls not come back in the agent's latest entries (notes may have
// come since): the last ones it made, those it may be asking about. None
// once its latest entry ended otherwise (it wrote, the call came back).
export function pendingTools(entries: ConvoEntry[]): ConvoTool[] {
  const out: ConvoTool[] = []
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i]
    if (e.role === 'note') continue
    if (e.role !== 'assistant') break
    for (let j = e.parts.length - 1; j >= 0; j--) {
      const tl = e.parts[j].tool
      if (!tl) return out
      if (!tl.done && !asksNothing.has(tl.name)) out.push(tl)
    }
  }
  return out
}

// The one of them the screen's prompt is of; none when it is of none, or
// of more than one (calls made together, of the same file, say).
export function askedTool(tools: ConvoTool[], screen: string): ConvoTool | undefined {
  const hits = tools.filter((tl) => onScreen(tl, screen))
  return hits.length === 1 ? hits[0] : undefined
}

// Text as compared: no spaces, no box drawing, no line ends (a long command
// wraps on the screen, inside a box).
const squash = (s: string) => s.replace(/[\s│|╭╮╰╯─━┌┐└┘╌]+/g, '')

// Whether the screen's prompt is of this call: it shows the command, or the
// file's name, or the address.
export function onScreen(tool: ConvoTool, screen: string): boolean {
  const d = tool.detail ?? ''
  if (!d) return false
  // (The prompt's own lines, at the end: not the transcript above them.)
  const s = squash(screen.split('\n').slice(-24).join('\n'))
  switch (tool.name) {
    case 'Bash': {
      const head = squash(d.split('\n')[0]).slice(0, 40)
      return head.length >= 2 && s.includes(head)
    }
    case 'Edit':
    case 'MultiEdit':
    case 'Write':
    case 'NotebookEdit': {
      const base = d.split('/').filter(Boolean).pop() ?? ''
      return base.length > 0 && s.includes(squash(base))
    }
    case 'WebFetch': {
      let host = ''
      try {
        host = new URL(d).host
      } catch {
        return false
      }
      return !!host && s.includes(host)
    }
  }
  return false
}

// A choice Claude Code offers, as the person reads it: in Chinese the usual
// ones, with the rest of the line kept as it is (a command, a folder); else
// as on the screen.
const zhChoices: [RegExp, (m: RegExpExecArray) => string][] = [
  [/^Yes$/i, () => '允许'],
  [/^No$/i, () => '拒绝'],
  [/^No,? and tell Claude what to do differently(?: \(esc\))?$/i, () => '拒绝，并告诉 Claude 怎么做'],
  [/^Yes,? and don['’]t ask again for:?\s+(.+?) commands in (.+)$/i, (m) => `允许，以后在 ${m[2]} 里运行 ${m[1]} 不再问`],
  [/^Yes,? and don['’]t ask again for:?\s+(.+)$/i, (m) => `允许，以后 ${m[1]} 不再问`],
  [/^Yes,? and switch to accept edits\b.*$/i, () => '允许，并在这个会话里自动接受修改'],
  [/^Yes,? allow all edits (?:during|in) this session\b.*$/i, () => '允许，这个会话里改文件都不再问'],
  [/^Yes,? and switch to auto mode\b.*$/i, () => '允许，并切到自动模式'],
]

export function choiceText(label: string): { text: string; original?: string } {
  if (getLang() !== 'zh-CN') return { text: label }
  for (const [re, f] of zhChoices) {
    const m = re.exec(label.trim())
    if (m) return { text: f(m), original: label }
  }
  return { text: label }
}

// The question, likewise.
const zhQuestions: [RegExp, (m: RegExpExecArray) => string][] = [
  [/^Do you want to proceed\?$/i, () => '要继续吗？'],
  [/^Do you want to make this edit to (.+)\?$/i, (m) => `要修改 ${m[1]} 吗？`],
  [/^Do you want to create (.+)\?$/i, (m) => `要新建 ${m[1]} 吗？`],
  [/^Do you want to make these changes\?$/i, () => '要做这些修改吗？'],
  [/^Do you want to allow Claude to fetch this content\?$/i, () => '要让 Claude 打开这个网址吗？'],
]

export function questionText(q: string): string {
  if (getLang() !== 'zh-CN') return q
  for (const [re, f] of zhQuestions) {
    const m = re.exec(q.trim())
    if (m) return f(m)
  }
  return q
}
