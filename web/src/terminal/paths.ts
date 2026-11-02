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
// one; a line number after a file's name, "file.go:42", left off — not
// after a name of no extension: "backup-12:30" may be the name). Paths in
// the prose itself are left as they are — too much there only looks like
// one (an address's path, a route, one on another host).
export function codePath(s: string): string | null {
  const t = s.trim()
  if (!/^\/(?!\/)[^\s`'"<>|;&$]{0,1023}$/.test(t) || t === '/' || /\/\.{1,2}(\/|$)/.test(t)) return null
  const m = /^(.*\/[^/]*\.[^/.:]+)(?::\d+(?::\d+)?)$/.exec(t)
  return m ? m[1] : t
}
