// Leave-page confirmation (beforeunload): guards against closing the tab or
// installed-app window by mistake (e.g. Ctrl+W / Cmd+W) while a terminal
// session is open or an input box holds unsent text. Self-initiated reloads
// and redirects call allowLeave() / reloadPage() so they are never blocked.

export interface LeaveGuardInput {
  enabled: boolean
  bypassUntil: number
  now: number
  openTerminals: number
  hasDrafts: boolean
}

const ACTIVE_TERMINAL_STATUSES = new Set(['connecting', 'open', 'reconnecting', 'offline'])

export function isActiveTerminalStatus(status: string): boolean {
  return ACTIVE_TERMINAL_STATUSES.has(status)
}

export function shouldConfirmLeave(input: LeaveGuardInput): boolean {
  if (!input.enabled) return false
  if (input.now < input.bypassUntil) return false
  return input.openTerminals > 0 || input.hasDrafts
}

const activeTerminals = new Set<symbol>()
const draftChecks = new Set<() => boolean>()
let bypassUntil = 0

export function setLeaveGuardTerminal(token: symbol, active: boolean) {
  if (active) activeTerminals.add(token)
  else activeTerminals.delete(token)
}

export function registerDraftCheck(check: () => boolean): () => void {
  draftChecks.add(check)
  return () => draftChecks.delete(check)
}

export function hasTrackedDrafts(): boolean {
  for (const check of draftChecks) if (check()) return true
  return false
}

// Mark an intentional page reload or navigation so beforeunload does not prompt.
// Expires automatically if the browser aborts or ignores the navigation.
export function allowLeave(windowMs = 5000) {
  bypassUntil = Date.now() + windowMs
}

export function reloadPage() {
  allowLeave()
  location.reload()
}

export function installLeaveGuard(isEnabled: () => boolean): () => void {
  const onBeforeUnload = (e: BeforeUnloadEvent) => {
    if (
      !shouldConfirmLeave({
        enabled: isEnabled(),
        bypassUntil,
        now: Date.now(),
        openTerminals: activeTerminals.size,
        hasDrafts: hasTrackedDrafts(),
      })
    )
      return
    e.preventDefault()
    e.returnValue = ''
  }
  window.addEventListener('beforeunload', onBeforeUnload)
  return () => window.removeEventListener('beforeunload', onBeforeUnload)
}
