import type { ReactNode } from 'react'
import { t } from '../i18n'
import type { ConvoHunk } from './conversation'

// An edit as Claude Code reported making it: its hunks with the lines'
// numbers (after the edit; before it, for a line removed), the lines left
// out between them said. (Also a file's diff as git reads it: the Changes
// view's; className: its look there.)
export function Hunks({ hunks, cut, className = '' }: { hunks: ConvoHunk[]; cut?: boolean; className?: string }) {
  const rows: ReactNode[] = []
  let end = 0 // (the new numbering just past the hunk before)
  hunks.forEach((h, hi) => {
    if (hi > 0 && h.new_start > end) rows.push(<div key={'g' + hi} className="convo-hunk-gap">{t('convo.unchanged', { n: h.new_start - end })}</div>)
    let o = h.old_start
    let n = h.new_start
    h.lines.forEach((l, li) => {
      const k = l[0]
      const no = k === '-' ? o++ : k === '+' ? n++ : (o++, n++)
      rows.push(
        <div key={hi + '.' + li} className={'convo-hunk-line' + (k === '+' ? ' is-add' : k === '-' ? ' is-del' : '')}>
          <span className="convo-hunk-no">{no}</span>
          <span className="convo-hunk-sign">{k === ' ' ? '' : k === '-' ? '−' : '+'}</span>
          <span className="convo-hunk-text">{l.slice(1)}</span>
        </div>,
      )
    })
    end = n
  })
  return (
    <div className={'convo-hunks ' + className}>
      {rows}
      {cut && <div className="convo-hunk-gap">{t('convo.patchCut')}</div>}
    </div>
  )
}
