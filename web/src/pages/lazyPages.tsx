import { useEffect, useState, type ComponentProps, type CSSProperties } from 'react'
import { t } from '../i18n'
import { termScheme, usePrefs } from '../prefs'
import { chromeVars } from '../terminal/themes'

// The pages that need the terminal emulator (xterm and its add-ons, a good
// part of the code), loaded when first needed rather than with the app: the
// hosts list and the agents page come up without them. Loaded beside as soon
// as the app is up (preloadTerminal), so that a terminal opened later is
// drawn at once: rendered from the module once it is here, never through a
// frame of a fallback in between (React's lazy would show one each time it
// first renders, the module loaded or not — a flash).

type TerminalModule = typeof import('./Terminal')
type RecordingsModule = typeof import('./Recordings')

let terminal: TerminalModule | null = null
let terminalLoading: Promise<TerminalModule> | null = null
export function preloadTerminal(): Promise<TerminalModule> {
  // (Failed, as offline: asked again next time, not kept as failed.)
  terminalLoading ??= import('./Terminal').then(
    (m) => (terminal = m),
    (e) => {
      terminalLoading = null
      throw e
    },
  )
  return terminalLoading
}

let recordings: RecordingsModule | null = null
let recordingsLoading: Promise<RecordingsModule> | null = null
function loadRecordings(): Promise<RecordingsModule> {
  recordingsLoading ??= import('./Recordings').then(
    (m) => (recordings = m),
    (e) => {
      recordingsLoading = null
      throw e
    },
  )
  return recordingsLoading
}

// useModule: the module once loaded (at once, if it is), loading it if not;
// failed: said so (a browser keeps a module's failure for the page: only a
// reload asks for it again).
function useModule<M>(now: M | null, load: () => Promise<M>): { mod: M | null; failed: boolean } {
  const [mod, setMod] = useState(now)
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    if (mod) return
    let live = true
    load().then(
      (m) => live && setMod(() => m),
      () => live && setFailed(true),
    )
    return () => {
      live = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  return { mod: mod ?? now, failed }
}

function LoadFailed() {
  return (
    <div className="lazy-failed" role="alert">
      <p>{t('app.partFailed')}</p>
      <button type="button" className="btn" onClick={() => location.reload()}>
        {t('app.reload')}
      </button>
    </div>
  )
}

export function TerminalPage(props: ComponentProps<TerminalModule['TerminalPage']>) {
  usePrefs() // (its colours, should they change meanwhile)
  const { mod: m, failed } = useModule(terminal, preloadTerminal)
  // (Its place held, in the terminal's colours, while it loads.)
  if (m) return <m.TerminalPage {...props} />
  const scheme = termScheme()
  return (
    <div className={'term-root ' + (scheme.dark ? 'dark' : 'light') + (props.active === false ? ' inactive' : '')} style={chromeVars(scheme) as CSSProperties} aria-busy={!failed}>
      {failed && <LoadFailed />}
    </div>
  )
}

export function RecordingsPage() {
  const { mod: m, failed } = useModule(recordings, loadRecordings)
  return m ? <m.RecordingsPage /> : failed ? <LoadFailed /> : <p className="loading">{t('common.loading')}</p>
}

export function RecordingPlayerPage(props: ComponentProps<RecordingsModule['RecordingPlayerPage']>) {
  const { mod: m, failed } = useModule(recordings, loadRecordings)
  return m ? <m.RecordingPlayerPage {...props} /> : failed ? <LoadFailed /> : <p className="loading">{t('common.loading')}</p>
}
