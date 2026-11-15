import { useEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from 'react'
import { Download, FileCode2, BookOpen, RotateCw } from 'lucide-react'
import { api, ApiError, errorText } from '../api'
import { t } from '../i18n'
import { Dialog, DialogHead, HeadTool } from '../components/ui'
import { isDark, usePrefs } from '../prefs'
import { Code } from './Code'
import { Markdown } from './markdown'
import { FileTools, fileWhere, useDownload } from './picture'
import { FileTypeIcon } from './fileIcons'
import { formatSize } from './FilesDialog'
import type { LineSpan } from './paths'

// A text file of the host looked at in the page (GET …/files/text: its
// words as JSON, up to a megabyte — a longer one's beginning, said so).
// Code coloured by its language where it is known; Markdown drawn, its
// source a tap away; anything else as it is. HTML and SVG only as their
// source: nothing of a file runs here.

export const IMAGE_EXT = /\.(png|jpe?g|gif|webp)$/i
// (What is surely no text: offered for download, not for a look.)
const NOT_TEXT = /\.(zip|gz|tgz|tar|bz2|xz|7z|rar|zst|jar|war|whl|deb|rpm|dmg|iso|img|exe|dll|so|dylib|a|o|class|pyc|wasm|bin|dat|db|sqlite3?|pdf|docx?|xlsx?|pptx?|odt|ods|key|numbers|pages|ico|bmp|tiff?|psd|heic|avif|mp[34]|m4a|wav|flac|ogg|mov|avi|mkv|webm|ttf|otf|woff2?|eot|pt|pth|safetensors|onnx|gguf|parquet|npy|npz|pkl)$/i

const baseName = (p: string) => p.slice(p.lastIndexOf('/') + 1)

// Whether a file of this name is looked at in the page: a picture, or what
// may be text (the host says for sure).
export const previewable = (path: string) => IMAGE_EXT.test(path) || !NOT_TEXT.test(baseName(path))

// The language its words are coloured as, by its name (highlight.ts's grammars).
const LANG: Record<string, string> = {
  ts: 'typescript', tsx: 'typescript', mts: 'typescript', cts: 'typescript', js: 'javascript', jsx: 'javascript', mjs: 'javascript', cjs: 'javascript',
  py: 'python', pyi: 'python', go: 'go', rs: 'rust', json: 'json', jsonc: 'json', jsonl: 'json', json5: 'json', yaml: 'yaml', yml: 'yaml',
  md: 'markdown', markdown: 'markdown', css: 'css', scss: 'css', less: 'css', html: 'xml', htm: 'xml', xml: 'xml', svg: 'xml', vue: 'xml',
  sh: 'bash', bash: 'bash', zsh: 'bash', sql: 'sql', toml: 'ini', ini: 'ini', cfg: 'ini', conf: 'ini', env: 'bash',
  c: 'c', h: 'c', cpp: 'cpp', hpp: 'cpp', cc: 'cpp', java: 'java', kt: 'java', diff: 'diff', patch: 'diff', mk: 'makefile',
}
const NAMED: Record<string, string> = { dockerfile: 'dockerfile', containerfile: 'dockerfile', makefile: 'makefile', gnumakefile: 'makefile', '.bashrc': 'bash', '.zshrc': 'bash', '.profile': 'bash', '.bash_profile': 'bash', '.env': 'bash' }
// (Own keys only: a file named "constructor" is no language — review.)
const own = (o: Record<string, string>, k: string) => (Object.hasOwn(o, k) ? o[k] : undefined)
export function langOf(path: string): string | undefined {
  const n = baseName(path).toLowerCase()
  const named = own(NAMED, n)
  if (named) return named
  if (n.startsWith('.env.')) return 'bash'
  const dot = n.lastIndexOf('.')
  return dot > 0 ? own(LANG, n.slice(dot + 1)) : undefined
}

type Read = { text: string; size: number; cut: boolean }

export function readError(e: unknown): string {
  switch (e instanceof ApiError ? e.code : '') {
    case 'not_text':
      return t('preview.notText')
    case 'file_not_found':
      return t('preview.gone')
    case 'not_a_file':
      return t('preview.notFile')
    case 'busy':
      return t('picture.busy')
    case 'session_ended':
      return t('preview.ended')
  }
  return errorText(e)
}

const MD_DRAWN_MAX = 300_000

// Its lines' numbers, one a line; those marked in a block of their own.
// (No line break after the block: it ends its line already — one there
// would be a line more, every number after it one off.)
function nums(n: number, marked: LineSpan | null): ReactNode {
  const run = (a: number, b: number) => Array.from({ length: Math.max(0, b - a + 1) }, (_, i) => a + i).join('\n')
  if (!marked) return run(1, n)
  return (
    <>
      {marked.from > 1 && run(1, marked.from - 1) + '\n'}
      <span className="is-marked">{run(marked.from, marked.to)}</span>
      {marked.to < n && run(marked.to + 1, n)}
    </>
  )
}

// tail: its end (a command's output, a log: the newest), read again on asking,
// shown scrolled to the bottom. lines: those a reference named ("a.go:12",
// "paradigm.md:82-103"), shown scrolled to and marked — a Markdown file then
// as its source, where its lines are (not with tail: its lines are not counted).
export function TextPreview({ sid, path, tail = false, lines: at0, onClose, onReveal }: { sid: string; path: string; tail?: boolean; lines?: LineSpan; onClose: () => void; onReveal?: () => void }) {
  usePrefs() // (its colours follow the theme)
  const download = useDownload()
  const [read, setRead] = useState<Read | null>(null)
  const [error, setError] = useState('')
  const [again, setAgain] = useState(0)
  const lang = langOf(path)
  const name0 = baseName(path)
  const at = tail ? undefined : at0
  const [source, setSource] = useState(!!at) // (Markdown: its source shown, not drawn)
  // (Another file or other lines asked for in the same view: as it opens.)
  const atFrom = at?.from
  const atTo = at?.to
  const first = useRef(true)
  useEffect(() => {
    if (first.current) first.current = false
    else setSource(atFrom !== undefined)
  }, [path, atFrom, atTo])
  useEffect(() => {
    const ctl = new AbortController()
    if (!tail) setRead(null) // (read again from its end: what is shown stays till the newer comes)
    setError('')
    api<Read>('GET', `/api/sessions/${sid}/files/text?path=` + encodeURIComponent(path) + (tail ? '&tail=1' : ''), undefined, { signal: ctl.signal }).then(
      (r) => !ctl.signal.aborted && setRead(r),
      (e) => !ctl.signal.aborted && setError(readError(e)),
    )
    return () => ctl.abort()
  }, [sid, path, again, tail])
  // (From its end: the bottom in sight, as it is read, and again.)
  const box = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (tail && read && box.current) box.current.scrollTop = box.current.scrollHeight
  }, [tail, read])
  const name = name0
  // (Markdown drawn up to so long; a longer one shown as its source: drawing it would hold the page.)
  const md = lang === 'markdown' && (!read || read.text.length <= MD_DRAWN_MAX)
  // (Its lines' numbers, beside them: as many as it has.)
  const lines = useMemo(() => (read ? read.text.split('\n').length - (read.text.endsWith('\n') ? 1 : 0) : 0), [read])
  // The lines asked for that it has (a file changed since, or cut at a
  // megabyte, may have fewer); marked, and scrolled to with a few above.
  const marked = at && at.from <= lines ? { from: at.from, to: Math.min(at.to, lines) } : null
  const asSource = !!read && read.text !== '' && !(md && !source)
  const band = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const b = box.current
    if (!b || !at || !asSource) return
    const m = band.current
    // (Past what is shown of a cut file: its end, where that is said.)
    if (!m) {
      if (read?.cut && at.from > lines) b.scrollTop = b.scrollHeight
      return
    }
    const room = b.clientHeight
    const h = m.offsetHeight
    const lh = h / (marked ? marked.to - marked.from + 1 : 1)
    // (A short range a third down; a long one from its first line, three lines above it in sight.)
    const above = h < room * 0.6 ? (room - h) / 3 : 3 * lh
    b.scrollTop += m.getBoundingClientRect().top - b.getBoundingClientRect().top - above
  }, [read, asSource, atFrom, atTo])
  return (
    <Dialog title={name} onClose={onClose} wide hideTitle className="dlg text-preview-dialog">
      <DialogHead
        icon={<FileTypeIcon name={name} />}
        title={name}
        titleText={path}
        oneLine
        sub={fileWhere(path, read ? formatSize(read.size) : undefined, at && (at.to > at.from ? t('preview.lines', { from: at.from, to: at.to }) : t('preview.line', { n: at.from })))}
        subText={path}
        tools={
          <FileTools
            sid={sid}
            path={path}
            onReveal={onReveal}
            extra={
              <>
                {tail && (
                  <HeadTool label={t('preview.reload')} onClick={() => setAgain((n) => n + 1)}>
                    <RotateCw size={16} />
                  </HeadTool>
                )}
                {/* (Drawn or its source: words, not an icon — a touch screen shows no hint.) */}
                {md && read && (
                  <button type="button" className="btn btn-ghost text-preview-mode" aria-pressed={source} onClick={() => setSource(!source)}>
                    {source ? <BookOpen size={15} /> : <FileCode2 size={15} />} {t(source ? 'preview.drawn' : 'preview.source')}
                  </button>
                )}
              </>
            }
          />
        }
        onClose={onClose}
      />
      <div className={'text-preview term-vars ' + (isDark() ? 'dark' : 'light')} ref={box}>
        {/* (Its end shown: what is left out is above.) */}
        {tail && read?.cut && <p className="text-preview-cut">{t('preview.cutTail', { all: formatSize(read.size) })}</p>}
        {error ? (
          <div className="picture-note" role="alert">
            <p>{error}</p>
            <span className="picture-acts">
              <button type="button" className="btn" onClick={() => setAgain((n) => n + 1)}>{t('common.retry')}</button>
              <button type="button" className="btn" onClick={() => download(sid, path)}><Download size={15} /> {t('convo.path.download')}</button>
            </span>
          </div>
        ) : !read ? (
          <p className="picture-note hint">{t('common.loading')}</p>
        ) : read.text === '' ? (
          <p className="picture-note hint">{t('preview.empty')}</p>
        ) : md && !source ? (
          <div className="text-preview-md convo-md"><Markdown text={read.text} soft /></div>
        ) : (
          <div className="text-preview-code">
            {/* (Lines counted from a cut-off start would not be the file's: none then.) */}
            {/* (The lines asked for: a band behind them, their numbers marked too — the numbers' column covers the band.) */}
            {marked && <div className="text-preview-mark" ref={band} aria-hidden="true" style={{ '--mark-at': marked.from - 1, '--mark-n': marked.to - marked.from + 1 } as CSSProperties} />}
            {!(tail && read.cut) && <pre className="text-preview-nums" aria-hidden="true">{nums(lines, marked)}</pre>}
            <pre className="text-preview-text"><Code text={read.text} lang={lang} /></pre>
          </div>
        )}
        {!tail && read?.cut && <p className="text-preview-cut is-end">{t('preview.cut', { all: formatSize(read.size) })}</p>}
      </div>
    </Dialog>
  )
}
