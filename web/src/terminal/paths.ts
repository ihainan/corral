// Paths as they are typed for a program: quoted for a shell, or Gemini's @path.

// Quote a remote path for a POSIX shell when it contains anything beyond safe
// characters. Generated upload names are safe; the home directory may not be.
export function shellPath(p: string): string | null {
  if (/[\x00-\x1f\x7f]/.test(p)) return null
  if (/^[\w@%+=:,./~-]+$/.test(p)) return p
  return "'" + p.replaceAll("'", "'\\''") + "'"
}

// geminiPath is a path as Gemini CLI takes a file: @path, where a backslash
// keeps a character that would end the path (it does not read shell quotes;
// tabs and line breaks are control characters, refused before).
export function geminiPath(p: string): string | null {
  if (/[\x00-\x1f\x7f]/.test(p)) return null
  return '@' + p.replace(/[ ,;!?()[\]{}"'`\\]/g, '\\$&').replace(/\.$/, '\\.')
}

// A code span of an answer that is a path and nothing else (an absolute
// one, or one under the home folder, "~/…", which the page makes whole
// with the host's home when it is acted on). Paths in the prose itself are
// left as they are — too much there only looks like one (an address's path,
// a route, one on another host).
// The lines it names after a file's name are taken apart from it, the file
// looked at there: ":12", ":12:5" (a column: not kept), ":82-103", or as
// Codex may link one, "#L12", "#L12C5", "#L12-L20". Not after another name
// of no extension: "backup-12:30" may be the name (which it is cannot be
// told from the words alone: this guesses). (link: the target of a file
// link of Codex's, where what follows the name is its lines whatever the
// name — it writes them so.)
export function codeRef(s: string, link = false): { path: string; lines?: LineSpan; rel?: boolean } | null {
  const t = s.trim()
  if (!/^(?:\/(?!\/)|~\/)[^\s`'"<>|;&$]{0,1023}$/.test(t) || t === '/' || t === '~/' || /\/\.{1,2}(\/|$)/.test(t)) return relRef(t, link)
  const m = LINES.exec(t)
  const path = m ? t.slice(0, m.index) : t
  if (!m || !(link ? /\/[^/]+$/ : NAMED_FILE).test(path)) return { path: t }
  const from = Number(m[1] ?? m[3])
  const to = Number(m[2] ?? m[4] ?? from)
  // (Line 0 is none: the file from its top.)
  if (!from) return { path }
  return { path, lines: { from: Math.min(from, to || from), to: Math.max(from, to) } }
}

// The lines a reference names in its file, first to last (one: both the same).
export type LineSpan = { from: number; to: number }

const LINES = /(?::(\d{1,7})(?::\d{1,7}|-(\d{1,7}))?|#L(\d{1,7})(?:C\d{1,7})?(?:-L?(\d{1,7})(?:C\d{1,7})?)?)$/
const NAMED_FILE = /\/(?:[^/]*\.[^/.:#]+|Makefile|Dockerfile|Containerfile|Rakefile|Gemfile|Jenkinsfile|Vagrantfile|BUILD|WORKSPACE|Justfile|justfile|makefile|GNUmakefile)$/

// A path relative to the agent's folder ("src/foo.ts:42", "paradigm.md:82-103"):
// taken only where it can be told for a file's — a folder before a name with
// an extension, or lines after one ("origin/main", "console.log", "v1.2",
// "*.ts" are not) — and, in a link of its own, any name. Made whole only when
// acted on, with the agent's folder as it is then, and looked for there
// first: the call may have been made in another (rel: so).
const REL = /^(?:\.\/)?(?![-@])[^\s`'"<>|;&$*?[\]{}\\]{1,1024}$/
const REL_FILE = /(?:^|\/)[^/]*[^/.]\.[^/.:#]*[A-Za-z][^/.:#]*$|(?:^|\/)(?:Makefile|Dockerfile|Containerfile|Justfile|justfile|makefile|GNUmakefile)$/
function relRef(t: string, link: boolean): { path: string; lines?: LineSpan; rel: true } | null {
  if (!REL.test(t) || t.includes('://') || t.startsWith('/') || t.startsWith('~') || /(^|\/)\.{1,2}(\/|$)/.test(t.replace(/^\.\//, ''))) return null
  const m = LINES.exec(t)
  const path = (m ? t.slice(0, m.index) : t).replace(/^\.\//, '')
  if (!path || path.endsWith('/') || !REL_FILE.test(path) || (!m && !link && !path.includes('/'))) return null
  if (!m) return { path, rel: true }
  const from = Number(m[1] ?? m[3])
  const to = Number(m[2] ?? m[4] ?? from)
  if (!from) return { path, rel: true }
  return { path, lines: { from: Math.min(from, to || from), to: Math.max(from, to) }, rel: true }
}
