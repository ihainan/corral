// The menu an agent asks with, as its screen shows it.

// choices finds the menu an agent asks with at the end of the screen, as
// Claude Code, Codex and Gemini CLI draw it: numbered lines in a row, one of
// them marked as selected ("❯ 1. Yes", "  2. No"), followed by at most a few
// lines of blank space, borders or key hints ("Esc to cancel"). The number
// is the key that picks a choice. A numbered list without the mark
// (ordinary output), or one followed by anything else (a new question), is
// no menu.
const menuLine = /^[\s│|]*([❯›>▶→●◉])?\s*([1-9])[.)]\s+(\S.*?)\s*[│|]?\s*$/
const hintLine = /^[\s│|╭╮╰╯─━┌┐└┘]*$|\b(esc|cancel|confirm)\b/i
const maxLabel = 120

// A choice too long for the pane's width goes on in lines that begin where
// its text does (a narrow pane, a phone's). A key hint is none of them.
const moreLine = /^[\s│|]{3,}(\S.*?)\s*[│|]?\s*$/
const keyHint = /^[\s│|]*(esc|enter|tab|ctrl|shift|↑|↓|←|→)\b/i
const choiceHead = /^[\s│|]*(?:[❯›>▶→●◉])?\s*[1-9][.)]\s+/
const lead = (l: string) => /^[\s│|]*/.exec(l)![0].length

// Whether line i goes on with the choice above it.
function goesOn(lines: string[], i: number): boolean {
  if (!moreLine.test(lines[i]) || keyHint.test(lines[i])) return false
  for (let j = i - 1; j >= 0 && i - j <= 4; j--) {
    const head = choiceHead.exec(lines[j])
    if (head && menuLine.test(lines[j])) return lead(lines[i]) === head[0].length
    if (!moreLine.test(lines[j])) return false
  }
  return false
}

// The menu at the end of the screen's lines: its choices and the line it
// begins on.
function menuAt(lines: string[]): { out: { key: string; label: string }[]; top: number } {
  let end = lines.length
  while (end > 0 && !menuLine.test(lines[end - 1]) && hintLine.test(lines[end - 1]) && !goesOn(lines, end - 1) && lines.length - end < 3) end--
  const out: { key: string; label: string }[] = []
  let marked = 0
  let top = -1
  let more: string[] = [] // (lines going on from the choice above them)
  for (let i = end - 1; i >= 0; i--) {
    const m = menuLine.exec(lines[i])
    if (!m) {
      if (!goesOn(lines, i)) break
      more.unshift(moreLine.exec(lines[i])![1])
      continue
    }
    const label = [m[3], ...more].join(' ')
    more = []
    top = i
    out.unshift({ key: m[2], label: label.length > maxLabel ? label.slice(0, maxLabel - 1) + '…' : label })
    if (m[1]) marked++
  }
  const inOrder = out.every((c, i) => c.key === String(i + 1))
  return out.length >= 2 && inOrder && marked === 1 ? { out, top } : { out: [], top: -1 }
}

export function choices(text: string): { key: string; label: string }[] {
  return menuAt(text.split('\n')).out
}

// The question asked over a menu on the screen: the line above it that
// asks (ends in a question mark), else "".
export function question(text: string): string {
  const lines = text.split('\n')
  const { top } = menuAt(lines)
  for (let i = top - 1, n = 0; top > 0 && i >= 0 && n < 4; i--, n++) {
    const l = lines[i].replace(/^[\s│|╭╮╰╯─━┌┐└┘]+|[\s│|╭╮╰╯─━┌┐└┘]+$/g, '')
    if (!l) continue
    return /[?？]$/.test(l) && l.length <= 200 ? l : ''
  }
  return ''
}
