// Browser storage that never throws: private modes and blocked site data make
// localStorage accessors fail, and the app must keep working without them.

export const local = {
  get(key: string): string | null {
    try {
      return window.localStorage.getItem(key)
    } catch {
      return null
    }
  },
  set(key: string, value: string) {
    try {
      window.localStorage.setItem(key, value)
    } catch {
      /* unavailable or full */
    }
  },
  remove(key: string) {
    try {
      window.localStorage.removeItem(key)
    } catch {
      /* unavailable */
    }
  },
}

// This tab's own storage: kept across a reload, gone when the tab is closed.
export const session = {
  get(key: string): string | null {
    try {
      return window.sessionStorage.getItem(key)
    } catch {
      return null
    }
  },
  set(key: string, value: string) {
    try {
      window.sessionStorage.setItem(key, value)
    } catch {
      /* unavailable or full */
    }
  },
  remove(key: string) {
    try {
      window.sessionStorage.removeItem(key)
    } catch {
      /* unavailable */
    }
  },
}

// A list of names kept on this device (as JSON): none when unset, unreadable,
// or not a list of strings.
export function keptNames(key: string): string[] {
  try {
    const v: unknown = JSON.parse(local.get(key) ?? '[]')
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []
  } catch {
    return []
  }
}
export const keepNames = (key: string, names: string[]) => local.set(key, JSON.stringify(names))
