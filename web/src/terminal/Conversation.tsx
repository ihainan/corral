import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ArrowDown, Bot, ChevronRight, FilePen, FileText, Globe, Search, SquareTerminal, Wrench } from 'lucide-react'
import { t, getLang, type I18nKey } from '../i18n'
import { copyText } from '../components/ui'
import { Markdown } from './markdown'
import type { Convo, ConvoEntry, ConvoPart, ConvoTool } from './conversation'

// The conversation view: the agent's log as turns, over the terminal (which
// stays mounted under it). What the person wrote, the agent's answers as
// formatted text, and the tools it ran between them folded into one line
// per run of calls, opened on demand. Messages are still typed in the
// composer or the terminal: the view only reads.

type Item =
  | { kind: 'user'; e: ConvoEntry }
  | { kind: 'note'; e: ConvoEntry }
  | { kind: 'summary'; e: ConvoEntry }
  | { kind: 'text'; key: string; text: string; truncated?: boolean }
  | { kind: 'steps'; key: string; parts: ConvoPart[] }

function items(entries: ConvoEntry[]): Item[] {
  const out: Item[] = []
  let steps: ConvoPart[] | null = null
  for (const e of entries) {
    if (e.role !== 'assistant') {
      steps = null
      out.push({ kind: e.role, e } as Item)
      continue
    }
    e.parts.forEach((p, i) => {
      if (p.kind === 'text') {
        steps = null
        out.push({ kind: 'text', key: `${e.seq}.${i}`, text: p.text ?? '', truncated: p.truncated })
      } else if (p.kind === 'tool' || p.kind === 'thinking') {
        if (!steps) {
          steps = []
          out.push({ kind: 'steps', key: `${e.seq}.${i}`, parts: steps })
        }
        steps.push(p)
      }
    })
  }
  return out
}

type Category = 'cmd' | 'read' | 'edit' | 'search' | 'web' | 'agent' | 'other'

function category(name: string): Category {
  switch (name) {
    case 'Bash':
    case 'BashOutput':
      return 'cmd'
    case 'Read':
      return 'read'
    case 'Edit':
    case 'MultiEdit':
    case 'Write':
    case 'NotebookEdit':
      return 'edit'
    case 'Grep':
    case 'Glob':
    case 'WebSearch':
    case 'ToolSearch':
      return 'search'
    case 'WebFetch':
      return 'web'
    case 'Agent':
    case 'Task':
      return 'agent'
  }
  return 'other'
}

const icons: Record<Category, typeof Wrench> = { cmd: SquareTerminal, read: FileText, edit: FilePen, search: Search, web: Globe, agent: Bot, other: Wrench }

function count(cat: Category, n: number): string {
  return t(`convo.n.${cat}${n === 1 ? '1' : ''}` as I18nKey, { n })
}

export function ConversationView({
  convo,
  working,
  pending,
  onShowTerminal,
  onCopied,
}: {
  convo: Convo
  working: boolean
  pending: string | null
  onShowTerminal: () => void
  onCopied: () => void
}) {
  const list = useMemo(() => items(convo.entries), [convo.entries])
  const box = useRef<HTMLDivElement>(null)
  const stuck = useRef(true) // following the end
  const [away, setAway] = useState(false)
  const copy = (s: string) => void copyText(s).then((ok) => ok && onCopied())

  // Before the new content is laid out, note whether the end was in view;
  // after, keep it there.
  useLayoutEffect(() => {
    const el = box.current
    if (el && stuck.current) el.scrollTop = el.scrollHeight
  }, [list, pending, working])
  useEffect(() => {
    const el = box.current
    if (!el) return
    const onScroll = () => {
      const atEnd = el.scrollHeight - el.scrollTop - el.clientHeight < 48
      stuck.current = atEnd
      setAway(!atEnd)
    }
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => el.removeEventListener('scroll', onScroll)
  }, [])

  let body: ReactNode
  if (convo.status === 'loading') body = <p className="convo-state">{t('convo.loading')}</p>
  else if (convo.status === 'error') body = <p className="convo-state">{t('convo.error')}</p>
  else if (convo.status === 'none')
    body = (
      <div className="convo-state">
        <p>{t(convo.reason === 'no_log' ? 'convo.noLog' : convo.reason === 'unsupported' ? 'convo.unsupported' : 'convo.noAgent')}</p>
        <button type="button" className="btn btn-ghost" onClick={onShowTerminal}>{t('convo.showTerminal')}</button>
      </div>
    )
  else
    body = (
      <>
        {convo.older && <p className="convo-older">{t('convo.older')}</p>}
        {list.map((it) => {
          switch (it.kind) {
            case 'user':
              return <UserTurn key={it.e.seq} e={it.e} onCopy={copy} />
            case 'note':
              return (
                <p key={it.e.seq} className="convo-note">
                  {it.e.parts.map((p) => p.text).join(' ')}
                </p>
              )
            case 'summary':
              return (
                <details key={it.e.seq} className="convo-summary">
                  <summary>{t('convo.summary')}</summary>
                  <div className="convo-md">
                    <Markdown text={it.e.parts[0]?.text ?? ''} />
                  </div>
                </details>
              )
            case 'text':
              return (
                <div key={it.key} className="convo-md">
                  <Markdown text={it.text} onCopyCode={copy} copyLabel={t('common.copy')} />
                  {it.truncated && <p className="convo-cut">{t('convo.cut')}</p>}
                </div>
              )
            case 'steps':
              return <Steps key={it.key} parts={it.parts} onCopy={copy} />
          }
        })}
        {pending && (
          <div className="convo-user convo-pending">
            <div className="convo-user-text">{pending}</div>
            <div className="convo-meta">{t('convo.sent')}</div>
          </div>
        )}
        {working && <p className="convo-working">{t('convo.working')}</p>}
      </>
    )

  return (
    <div className="convo" role="region" aria-label={t('convo.title')}>
      <div className="convo-scroll" ref={box}>
        <div className="convo-col">{body}</div>
      </div>
      {away && convo.status === 'ready' && (
        <button
          type="button"
          className="convo-latest"
          onClick={() => {
            const el = box.current
            if (el) el.scrollTo({ top: el.scrollHeight, behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' })
          }}
        >
          <ArrowDown size={15} aria-hidden="true" />
          {t('convo.latest')}
        </button>
      )}
    </div>
  )
}

function clock(ts?: string): string {
  if (!ts) return ''
  const d = new Date(ts)
  if (isNaN(d.getTime())) return ''
  return d.toLocaleTimeString(getLang(), { hour: '2-digit', minute: '2-digit' })
}

function UserTurn({ e, onCopy }: { e: ConvoEntry; onCopy: (s: string) => void }) {
  const text = e.parts
    .filter((p) => p.kind === 'text')
    .map((p) => p.text)
    .join('\n')
  const images = e.parts.filter((p) => p.kind === 'image').length
  return (
    <div className="convo-user">
      <div className="convo-user-text">
        {text}
        {images > 0 && !/\[Image #\d+\]/.test(text) && <span className="convo-image">{t(images === 1 ? 'convo.images1' : 'convo.images', { n: images })}</span>}
      </div>
      <div className="convo-meta">
        <span>{clock(e.ts)}</span>
        {text && (
          <button type="button" className="link-btn" onClick={() => onCopy(text)}>
            {t('common.copy')}
          </button>
        )}
      </div>
    </div>
  )
}

function Steps({ parts, onCopy }: { parts: ConvoPart[]; onCopy: (s: string) => void }) {
  const [open, setOpen] = useState(false)
  const tools = parts.filter((p) => p.tool).map((p) => p.tool!)
  const counts = new Map<Category, number>()
  for (const tl of tools) counts.set(category(tl.name), (counts.get(category(tl.name)) ?? 0) + 1)
  const order: Category[] = ['cmd', 'read', 'edit', 'search', 'web', 'agent', 'other']
  let label = order
    .filter((c) => counts.has(c))
    .map((c) => count(c, counts.get(c)!))
    .join(t('convo.sep'))
  if (!label) label = t('convo.thinking')
  const failed = tools.filter((tl) => tl.error).length
  const running = tools.some((tl) => !tl.done)
  const last = [...tools].reverse().find((tl) => tl.title || tl.detail)
  const sub = last ? (last.title || firstLine(last.detail ?? '')) : ''
  return (
    <div className="convo-steps">
      <button type="button" className="convo-steps-head" aria-expanded={open} onClick={() => setOpen(!open)}>
        <ChevronRight size={15} className="convo-chev" aria-hidden="true" />
        <span className="convo-steps-text">
          <span className="convo-steps-label">
            {label}
            {failed > 0 && <span className="convo-failed">{t('convo.failed', { n: failed })}</span>}
            {running && <span className="convo-running">{t('convo.running')}</span>}
          </span>
          {sub && !open && <span className="convo-steps-sub">{sub}</span>}
        </span>
      </button>
      {open && (
        <div className="convo-steps-body">
          {parts.map((p, i) => (p.tool ? <ToolRow key={i} tool={p.tool} onCopy={onCopy} /> : <Thinking key={i} text={p.text ?? ''} />))}
        </div>
      )}
    </div>
  )
}

function firstLine(s: string): string {
  const l = s.split('\n').find((x) => x.trim()) ?? ''
  return l.length > 160 ? l.slice(0, 160) + '…' : l
}

function Thinking({ text }: { text: string }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="convo-tool">
      <button type="button" className="convo-tool-head" aria-expanded={open} onClick={() => setOpen(!open)}>
        <ChevronRight size={14} className="convo-chev" aria-hidden="true" />
        <span className="convo-tool-name">{t('convo.thinking')}</span>
        {!open && <span className="convo-tool-sub">{firstLine(text)}</span>}
      </button>
      {open && <div className="convo-thinking">{text}</div>}
    </div>
  )
}

function ToolRow({ tool, onCopy }: { tool: ConvoTool; onCopy: (s: string) => void }) {
  const [open, setOpen] = useState(false)
  const [full, setFull] = useState(false)
  const Icon = icons[category(tool.name)]
  const sub = tool.title || firstLine(tool.detail ?? '')
  const isEdit = tool.old !== undefined || tool.new !== undefined
  return (
    <div className={'convo-tool' + (tool.error ? ' is-error' : '')}>
      <button type="button" className="convo-tool-head" aria-expanded={open} onClick={() => setOpen(!open)}>
        <Icon size={14} className="convo-tool-icon" aria-hidden="true" />
        <span className="convo-tool-name">{tool.name}</span>
        <span className="convo-tool-sub">{sub}</span>
        {tool.error && <span className="convo-failed">{t('convo.toolFailed')}</span>}
      </button>
      {open && (
        <div className="convo-tool-body">
          {tool.detail && (tool.title || tool.detail.includes('\n') || tool.detail.length > 60) && (
            <div className="convo-pre-wrap">
              <pre className="convo-pre">{tool.detail}</pre>
              <button type="button" className="md-code-copy" onClick={() => onCopy(tool.detail!)}>{t('common.copy')}</button>
            </div>
          )}
          {isEdit && <Diff old={tool.old ?? ''} new={tool.new ?? ''} />}
          {tool.done ? (
            tool.result ? (
              <>
                <div className="convo-result-head">
                  {t('convo.output', { n: tool.result_lines ?? 0 })}
                  {tool.truncated && <span> · {t('convo.cut')}</span>}
                </div>
                <pre className={'convo-pre convo-result' + (full ? ' is-full' : '')}>{tool.result}</pre>
                {!full && (tool.result.split('\n').length > 12) && (
                  <button type="button" className="link-btn" onClick={() => setFull(true)}>{t('convo.showAll')}</button>
                )}
              </>
            ) : (
              <div className="convo-result-head">{t('convo.noOutput')}</div>
            )
          ) : (
            <div className="convo-result-head">{t('convo.running')}</div>
          )}
        </div>
      )}
    </div>
  )
}

function Diff({ old, new: nw }: { old: string; new: string }) {
  const rows: [string, string][] = []
  if (old) for (const l of old.split('\n')) rows.push(['del', l])
  if (nw) for (const l of nw.split('\n')) rows.push(['add', l])
  const shown = rows.slice(0, 60)
  return (
    <pre className="convo-pre convo-diff">
      {shown.map(([k, l], i) => (
        <span key={i} className={'convo-' + k}>
          {(k === 'del' ? '- ' : '+ ') + l + '\n'}
        </span>
      ))}
      {rows.length > shown.length && <span className="convo-more">{t('convo.moreLines', { n: rows.length - shown.length })}</span>}
    </pre>
  )
}
