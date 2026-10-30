import { getLang, t } from '../i18n'

// The words of an agent's permission prompt as the person reads them.

// A choice Claude Code offers, as the person reads it: in Chinese the usual
// ones, with the rest of the line kept as it is (a command, a folder); else
// as on the screen.
const zhChoices: [RegExp, (m: RegExpExecArray) => string][] = [
  // (Codex's, 0.164: its key in brackets at the end.)
  [/^Yes,? proceed(?: \(y\))?$/i, () => '允许这一次'],
  [/^Yes,? and don['’]t ask again for commands that start with `(.+)`(?: \(p\))?$/i, (m) => `允许，以后以 ${m[1]} 开头的命令不再问`],
  [/^Yes,? and don['’]t ask again for these files(?: \(a\))?$/i, () => '允许，这些文件以后不再问'],
  [/^No,? and tell Codex what to do differently(?: \(esc\))?$/i, () => '拒绝，并告诉 Codex 怎么做'],
  [/^None of the above$/i, () => '都不是'],
  [/^Yes$/i, () => '允许这一次'],
  [/^No$/i, () => '拒绝'],
  [/^No,? and tell Claude what to do differently(?: \(esc\))?$/i, () => '拒绝，并告诉 Claude 怎么做'],
  [/^Yes,? and don['’]t ask again for:?\s+(.+?) commands in (.+)$/i, (m) => `允许，以后在 ${m[2]} 里运行 ${m[1]} 不再问`],
  [/^Yes,? and don['’]t ask again for:?\s+(.+)$/i, (m) => `允许，以后 ${m[1]} 不再问`],
  [/^Yes,? and switch to accept edits\b.*$/i, () => '允许，并在这个会话里自动接受修改'],
  [/^Yes,? allow all edits (?:during|in) this session\b.*$/i, () => '允许，这个会话里改文件都不再问'],
  [/^Yes,? and switch to auto mode\b.*$/i, () => '允许，并切到自动模式'],
  [/^Yes,? and always allow access to (.+?) from this project$/i, (m) => `允许，以后在这个项目里访问 ${m[1]} 不再问`],
  // (A plan put to the person.)
  [/^Yes,? and use auto mode$/i, () => '批准，并切到自动模式'],
  [/^Yes,? manually approve edits$/i, () => '批准，修改仍逐个确认'],
  [/^Tell Claude what to change$/i, () => '告诉 Claude 要改什么'],
]

// What a choice does beyond this once, by Claude Code's words (whatever the
// language shown): a rule kept (not asked again), or the session's
// permission mode changed.
export function lasting(label: string): 'rule' | 'mode' | undefined {
  const l = label.trim()
  if (/^Yes,? and (?:switch to|use) (?:accept edits|auto mode)\b|^Yes,? allow all edits (?:during|in) this session\b/i.test(l)) return 'mode'
  if (/^Yes,? and (?:don['’]t ask again|always allow)\b/i.test(l)) return 'rule'
  return undefined
}

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
  [/^Would you like to run the following command\?$/i, () => '要运行这条命令吗？'],
  [/^Would you like to make the following edits\?$/i, () => '要做这些修改吗？'],
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

// An agent's name as the person reads it (claude, codex…: its process's).
export function agentName(agent?: string): string {
  switch (agent) {
    case 'claude':
      return 'Claude'
    case 'codex':
      return 'Codex'
    case 'opencode':
      return 'OpenCode'
    case 'gemini':
      return 'Gemini'
  }
  return t('convo.agentAny')
}
