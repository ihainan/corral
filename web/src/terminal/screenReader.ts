import { useEffect, useState } from 'react'
import { local } from '../storage'

// The live terminal read to a screen reader (xterm's screen reader mode:
// its rows kept as text the reader can go through, what is printed said):
// for whoever uses one on this device — kept here, not with the account's
// settings, as it is about the device and its reader, and costs the page
// more to draw while on (pages-1105 4.5). Off unless turned on.
const KEY = 'corral.term.screenReader'
const listeners = new Set<(on: boolean) => void>()

export const screenReaderOn = () => local.get(KEY) === '1'

export function setScreenReader(on: boolean) {
  local.set(KEY, on ? '1' : '0')
  listeners.forEach((f) => f(on))
}

export function useScreenReader(): boolean {
  const [on, setOn] = useState(screenReaderOn)
  useEffect(() => {
    listeners.add(setOn)
    return () => void listeners.delete(setOn)
  }, [])
  return on
}
