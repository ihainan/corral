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
