import { isDark, usePrefs } from '../prefs'
import { ICON_EXT, ICON_LIGHT, ICON_NAME } from './fileIconMap'

// A file's icon by its name, as VS Code's Material Icon Theme draws it
// (material-icon-theme 5.40.0, MIT: a hundred or so of its icons, picked by
// scripts out of its manifest — fileIconMap.ts). Files of their own, fetched
// as pictures: not drawn into the page's script.
const urls = import.meta.glob<string>('../assets/file-icons/*.svg', { eager: true, query: '?no-inline', import: 'default' })
const urlOf = (icon: string) => urls[`../assets/file-icons/${icon}.svg`]

// iconOf: the icon's name for a file (its whole name first — Dockerfile,
// package.json —, then its extension), a folder; "file" for any other.
// (Own keys only: a file named "constructor" is no icon — review. A name
// that begins with a dot is its own extension: .env, .dockerignore.)
const own = (o: Record<string, string>, k: string) => (Object.hasOwn(o, k) ? o[k] : undefined)
export function iconOf(name: string, dir = false): string {
  if (dir) return 'folder'
  const n = name.toLowerCase()
  const named = own(ICON_NAME, n)
  if (named) return named
  if (n.startsWith('.env.')) return own(ICON_EXT, 'env') ?? 'file'
  const dot = n.lastIndexOf('.')
  return (dot >= 0 && own(ICON_EXT, n.slice(dot + 1))) || 'file'
}

export function FileTypeIcon({ name, dir = false, size = 18 }: { name: string; dir?: boolean; size?: number }) {
  usePrefs() // (drawn again when the theme changes: some have a darker drawing for a light one)
  const icon = iconOf(name, dir)
  const src = (!isDark() && ICON_LIGHT[icon] && urlOf(ICON_LIGHT[icon])) || urlOf(icon) || urlOf('file')
  return <img className="file-type-icon" src={src} width={size} height={size} alt="" draggable={false} />
}
