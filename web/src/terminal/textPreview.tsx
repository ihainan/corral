import { useEffect, useMemo, useState } from 'react'
import { Copy, Download, FileCode2, BookOpen, FolderOpen, X } from 'lucide-react'
import { api, ApiError, errorText } from '../api'
import { t } from '../i18n'
import { copyText, Dialog, useToast } from '../components/ui'
import { isDark, usePrefs } from '../prefs'
import { Code } from './Code'
import { Markdown } from './markdown'
import { downloadPath } from './picture'
import { FileTypeIcon } from './fileIcons'
import { formatSize } from './FilesDialog'

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
export function langOf(path: string): string | undefined {
  const n = baseName(path).toLowerCase()
  if (NAMED[n]) return NAMED[n]
  if (n.startsWith('.env.')) return 'bash'
  const dot = n.lastIndexOf('.')
  return dot > 0 ? LANG[n.slice(dot + 1)] : undefined
}

type Read = { text: string; size: number; cut: boolean }

function readError(e: unknown): string {
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

export function TextPreview({ sid, path, onClose, onReveal }: { sid: string; path: string; onClose: () => void; onReveal?: () => void }) {
  usePrefs() // (its colours follow the theme)
  const toast = useToast()
  const [read, setRead] = useState<Read | null>(null)
  const [error, setError] = useState('')
  const [again, setAgain] = useState(0)
  const lang = langOf(path)
  const md = lang === 'markdown'
  const [source, setSource] = useState(false) // (Markdown: its source shown, not drawn)
  useEffect(() => {
    const ctl = new AbortController()
    setRead(null)
    setError('')
    api<Read>('GET', `/api/sessions/${sid}/files/text?path=` + encodeURIComponent(path), undefined, { signal: ctl.signal }).then(
      (r) => !ctl.signal.aborted && setRead(r),
      (e) => !ctl.signal.aborted && setError(readError(e)),
    )
    return () => ctl.abort()
  }, [sid, path, again])
  const name = baseName(path)
  // (Its lines' numbers, beside them: as many as it has.)
  const lines = useMemo(() => (read ? read.text.split('\n').length - (read.text.endsWith('\n') ? 1 : 0) : 0), [read])
  return (
    <Dialog title={name} onClose={onClose} wide hideTitle className="text-preview-dialog">
      <div className="picture-head">
        <span className="text-preview-icon" aria-hidden="true"><FileTypeIcon name={name} /></span>
        <h2 className="picture-name" title={name}>{name}</h2>
        {read && <span className="text-preview-size">{formatSize(read.size)}</span>}
        {md && read && (
          <button type="button" className="btn btn-ghost text-preview-mode" aria-pressed={source} onClick={() => setSource(!source)}>
            {source ? <BookOpen size={15} /> : <FileCode2 size={15} />} {t(source ? 'preview.drawn' : 'preview.source')}
          </button>
        )}
        <button type="button" className="icon-btn" onClick={onClose} aria-label={t('common.close')}><X size={18} /></button>
      </div>
      <div className={'text-preview term-vars ' + (isDark() ? 'dark' : 'light')}>
        {error ? (
          <div className="picture-note" role="alert">
            <p>{error}</p>
            <span className="picture-acts">
              <button type="button" className="btn" onClick={() => setAgain((n) => n + 1)}>{t('common.retry')}</button>
              <button type="button" className="btn" onClick={() => downloadPath(sid, path)}><Download size={15} /> {t('convo.path.download')}</button>
            </span>
          </div>
        ) : !read ? (
          <p className="picture-note hint">{t('common.loading')}</p>
        ) : read.text === '' ? (
          <p className="picture-note hint">{t('preview.empty')}</p>
        ) : md && !source ? (
          <div className="text-preview-md convo-md"><Markdown text={read.text} /></div>
        ) : (
          <div className="text-preview-code">
            <pre className="text-preview-nums" aria-hidden="true">{Array.from({ length: lines }, (_, i) => i + 1).join('\n')}</pre>
            <pre className="text-preview-text"><Code text={read.text} lang={lang} /></pre>
          </div>
        )}
        {read?.cut && <p className="text-preview-cut">{t('preview.cut', { all: formatSize(read.size) })}</p>}
      </div>
      <div className="picture-foot">
        <span className="picture-path" title={path}>{path}</span>
        <span className="picture-acts">
          <button
            type="button"
            className="btn btn-ghost"
            onClick={async () => {
              if (await copyText(path)) toast(t('common.copied'))
            }}
          >
            <Copy size={15} /> {t('convo.path.copy')}
          </button>
          {onReveal && (
            <button type="button" className="btn btn-ghost" onClick={onReveal}>
              <FolderOpen size={15} /> {t('convo.path.reveal')}
            </button>
          )}
          <button type="button" className="btn btn-ghost" onClick={() => downloadPath(sid, path)}>
            <Download size={15} /> {t('convo.path.download')}
          </button>
        </span>
      </div>
    </Dialog>
  )
}
