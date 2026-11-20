// Claude Code's notice/confirmation dialogs (<Dialog> / xe, e.g. the auto-mode
// classifier billing notice or end-of-check offer): drawn under a horizontal
// rule with a title, wrapped prose paragraphs, and a key-hint footer ("Enter
// to continue · Esc to cancel") rather than a numbered menu or cursor list.
// Read off the ANSI-stripped screen so the chat page can show a readable card
// with clear buttons instead of dropping the user into a raw terminal panel.
// Anything interactive (cursor rows, numbered menus, navigation/search/tab
// hints) returns null and stays in PanelWindow.

export type NoticeKey = 'enter' | 'shift+tab' | 'space' | 'esc'

export interface NoticeAction {
  key: NoticeKey
  hint: string // raw English verb from the footer (localized at render time where known)
  disabled?: boolean // e.g. "(needs a taller window)" on Claude Code's auto_mode_end_offer
}

export interface NoticeDialog {
  title: string
  paragraphs: string[]
  actions: NoticeAction[]
  clipped?: boolean // true when the pane was too short to show the footer (known notice fallback)
}

const RULE = /^\s*[─━]{8,}\s*$/
const CURSOR_LINE = /^\s*[❯›]/
const NUMBERED_LINE = /^\s*\d{1,2}[.)]\s+\S/
const FOOTER_PART = /^(Enter(?:\/Space)?|Shift\+Tab|Space|Esc)\s+to\s+(.+)$/i
const INTERACTIVE_HINT = /\b(?:navigate|select|switch|filter|search|scroll|toggle|cycle|edit in|go back)\b|[↑↓←→]/i
const TALLER_NOTE = /\s*\(needs a taller window\)\s*$/i
const CJK = /[\u3000-\u9fff\uff00-\uffef]/

// Known Claude Code notices whose body is tall enough to push the footer below
// the visible rows on a narrow/short phone pane. Only dialogs whose keys work
// unconditionally (or with height-gated keys omitted) belong here.
const CLIPPED_KNOWN: Record<string, NoticeAction[]> = {
  "We're changing auto mode to no longer charge for classifier requests in Claude Code.": [
    { key: 'enter', hint: 'continue' },
    { key: 'esc', hint: 'cancel' },
  ],
  "Auto mode can't check actions in this session": [
    // (In Claude Code's uIe, Shift+Tab is disabled when the window is too short,
    // so only Enter and Esc are offered when clipped.)
    { key: 'enter', hint: 'stay in auto mode' },
    { key: 'esc', hint: 'cancel' },
  ],
}

function joinLines(lines: string[]): string {
  let out = ''
  for (const raw of lines) {
    const s = raw.trim()
    if (!s) continue
    if (!out) {
      out = s
      continue
    }
    // Wrapped URL or path token without spaces, or CJK wrap: join without inserting a space.
    if (/https?:\/\/\S+$/.test(out) && !/\s/.test(s)) out += s
    else if (CJK.test(out.slice(-1)) || CJK.test(s[0])) out += s
    else out += ' ' + s
  }
  return out
}

function toParagraphs(lines: string[]): string[] {
  const groups: string[][] = []
  let cur: string[] = []
  for (const l of lines) {
    if (!l.trim()) {
      if (cur.length) {
        groups.push(cur)
        cur = []
      }
      continue
    }
    cur.push(l)
  }
  if (cur.length) groups.push(cur)
  return groups.map(joinLines).filter(Boolean)
}

function parseFooter(lines: string[]): NoticeAction[] | null {
  const joined = lines.map((l) => l.trim()).filter(Boolean).join(' ').replace(/\s+/g, ' ').trim()
  if (!joined || INTERACTIVE_HINT.test(joined)) return null
  const parts = joined.split(/\s*[·•]\s*/)
  if (!parts.length || parts.length > 3) return null
  const actions: NoticeAction[] = []
  const seen = new Set<NoticeKey>()
  for (const part of parts) {
    const m = FOOTER_PART.exec(part)
    if (!m) return null
    const rawKey = m[1].toLowerCase()
    const key: NoticeKey =
      rawKey === 'enter' || rawKey === 'enter/space'
        ? 'enter'
        : rawKey === 'shift+tab'
          ? 'shift+tab'
          : rawKey === 'space'
            ? 'space'
            : 'esc'
    if (seen.has(key)) return null
    seen.add(key)
    let hint = m[2].trim()
    const disabled = TALLER_NOTE.test(hint)
    if (disabled) hint = hint.replace(TALLER_NOTE, '').trim()
    if (!hint || INTERACTIVE_HINT.test(hint)) return null
    actions.push({ key, hint, ...(disabled ? { disabled: true } : {}) })
  }
  // Require at least one non-Esc action so info panels like /usage ("Esc to close")
  // stay in PanelWindow rather than being flattened into a notice card.
  if (!actions.some((a) => a.key !== 'esc')) return null
  return actions
}

export function readNoticeDialog(rawLines: string[]): NoticeDialog | null {
  const lines = rawLines.map((l) => l.replace(/\r/g, '').trimEnd())
  while (lines.length && !lines[lines.length - 1].trim()) lines.pop()
  if (lines.length < 3) return null

  let ruleIdx = -1
  for (let i = lines.length - 1; i >= 0; i--) {
    if (RULE.test(lines[i])) {
      ruleIdx = i
      break
    }
  }
  if (ruleIdx < 0 || ruleIdx >= lines.length - 1) return null

  const block = lines.slice(ruleIdx + 1)
  if (block.length > 45) return null

  // Reject any interactive prompt, picker, cursor list, or numbered menu.
  let numbered = 0
  for (const l of block) {
    if (CURSOR_LINE.test(l)) return null
    if (NUMBERED_LINE.test(l)) numbered++
  }
  if (numbered >= 2) return null

  // Path A: look for a key-hint footer at the bottom (1–3 wrapped lines).
  let footerStart = block.length
  while (footerStart > 0 && block[footerStart - 1].trim()) footerStart--
  const tail = block.slice(footerStart)
  if (tail.length >= 1 && tail.length <= 4) {
    const actions = parseFooter(tail)
    if (actions) {
      const paras = toParagraphs(block.slice(0, footerStart))
      if (paras.length >= 1 && paras[0].length >= 8) {
        return { title: paras[0], paragraphs: paras.slice(1), actions }
      }
    }
  }

  // Also handle a footer with no blank line above it (tight layout), only on the last 1–2 lines.
  for (let take = 1; take <= Math.min(2, block.length - 1); take++) {
    const cut = block.length - take
    const actions = parseFooter(block.slice(cut))
    if (actions) {
      const paras = toParagraphs(block.slice(0, cut))
      if (paras.length >= 1 && paras[0].length >= 8) {
        return { title: paras[0], paragraphs: paras.slice(1), actions }
      }
    }
  }

  // Path B: known tall Claude Code notice whose footer was clipped off on a short/narrow pane.
  const paras = toParagraphs(block)
  if (paras.length >= 1) {
    const known = CLIPPED_KNOWN[paras[0]]
    if (known) {
      return { title: paras[0], paragraphs: paras.slice(1), actions: known, clipped: true }
    }
  }

  return null
}
