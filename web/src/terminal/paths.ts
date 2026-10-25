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
