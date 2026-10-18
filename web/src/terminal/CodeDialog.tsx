import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { api, ApiError, errorText } from '../api'
import { hasKey, t } from '../i18n'
import { Dialog, useToast } from '../components/ui'

// VS Code in the browser (code-server on the host). Starting can take minutes
// the first time (download and unpack), so the server works in the
// background and we poll; the new tab is opened during the tap (so it is not
// treated as a pop-up) and pointed at VS Code once it is ready.

export interface CodeStatus {
  state: 'stopped' | 'checking' | 'downloading' | 'uploading' | 'extracting' | 'starting' | 'ready' | 'failed'
  error?: string
  message?: string
  url?: string
}

const working = (s: CodeStatus['state']) => s !== 'ready' && s !== 'failed' && s !== 'stopped'

function failureText(st: CodeStatus): string {
  const key = 'code.err.' + (st.error ?? '')
  if (st.error && hasKey(key)) return t(key as Parameters<typeof t>[0])
  return st.message ? t('code.failedDetail', { detail: st.message }) : t('code.failed')
}

interface Job {
  hostId: number
  name: string
  status: CodeStatus
  failure?: string
  // Ready, but the tab could not be used (blocked as a pop-up, or closed
  // while waiting): offer a link instead of leaving this page.
  readyUrl?: string
}

const REQUEST_TIMEOUT = 15000
const OVERALL_LIMIT = 15 * 60 * 1000 // a first install downloads and unpacks a few hundred MB

// useCode gives the "Open in VS Code" and "Stop VS Code" actions, the
// progress dialog to render, and which hosts are known to run VS Code.
export function useCode() {
  const toast = useToast()
  const [job, setJob] = useState<Job | null>(null)
  const [running, setRunning] = useState<Record<number, boolean>>({})
  // The current task. Each task has its own controller; callbacks of an
  // older task check it and do nothing once a newer one has started.
  const task = useRef<{ ctl: AbortController; win: Window | null } | null>(null)

  const endTask = (closeTab: boolean) => {
    const cur = task.current
    task.current = null
    cur?.ctl.abort()
    if (closeTab) cur?.win?.close()
  }
  useEffect(() => () => endTask(true), [])

  const markRunning = (hostId: number, on: boolean) => setRunning((r) => ({ ...r, [hostId]: on }))

  // refresh asks the server which of these hosts run VS Code.
  const refresh = useCallback((hostIds: number[]) => {
    for (const id of hostIds) {
      api<CodeStatus>('GET', `/api/hosts/${id}/code`, undefined, { timeoutMs: REQUEST_TIMEOUT })
        .then((st) => markRunning(id, st.state === 'ready'))
        .catch(() => {})
    }
  }, [])

  const open = (hostId: number, name: string) => {
    endTask(true)
    const win = window.open('about:blank', '_blank')
    // VS Code must not be able to navigate this tab.
    if (win) win.opener = null
    const mine = { ctl: new AbortController(), win }
    task.current = mine
    const current = () => task.current === mine
    const opts = { signal: mine.ctl.signal, timeoutMs: REQUEST_TIMEOUT }
    const started = Date.now()
    setJob({ hostId, name, status: { state: 'checking' } })

    const fail = (failure: string, status: CodeStatus = { state: 'failed' }) => {
      if (!current()) return
      win?.close()
      task.current = null
      setJob({ hostId, name, status, failure })
    }
    const run = async () => {
      let st = await api<CodeStatus>('POST', `/api/hosts/${hostId}/code`, undefined, opts)
      while (current() && working(st.state)) {
        if (Date.now() - started > OVERALL_LIMIT) return fail(t('code.tooLong'))
        setJob({ hostId, name, status: st })
        await new Promise((r) => setTimeout(r, 800))
        if (!current()) return
        st = await api<CodeStatus>('GET', `/api/hosts/${hostId}/code`, undefined, opts)
      }
      if (!current()) return
      if (st.state === 'ready' && st.url) {
        markRunning(hostId, true)
        task.current = null
        if (win && !win.closed) {
          win.location.href = st.url
          setJob(null)
        } else {
          setJob({ hostId, name, status: st, readyUrl: st.url })
        }
        return
      }
      fail(failureText(st), st)
    }
    run().catch((e) => {
      if (e instanceof ApiError && e.code === 'aborted') return
      fail(errorText(e))
    })
  }

  const stopCode = async (hostId: number) => {
    try {
      await api('POST', `/api/hosts/${hostId}/code/stop`)
      markRunning(hostId, false)
      toast(t('code.stopped'))
    } catch (e) {
      toast(e instanceof ApiError ? e.human : t('code.stopFailed'), true)
    }
  }

  const close = () => {
    endTask(true)
    setJob(null)
  }

  let dialog: ReactNode = null
  if (job) {
    const failed = !!job.failure
    dialog = (
      <Dialog title={t('code.title', { name: job.name })} onClose={close}>
        {failed ? (
          <p className="err" role="alert">{job.failure}</p>
        ) : job.readyUrl ? (
          <p className="lead" role="status">{t('code.readyOpenHere')}</p>
        ) : (
          <>
            <p className="lead" role="status" aria-live="polite">{t(('code.state.' + job.status.state) as Parameters<typeof t>[0])}</p>
            {(job.status.state === 'downloading' || job.status.state === 'uploading') && <p className="lead">{t('code.firstTime')}</p>}
            <p className="hint">{t('code.waitHint')}</p>
          </>
        )}
        <div className="dialog-actions">
          {failed && (
            <button className="btn" onClick={() => open(job.hostId, job.name)}>
              {t('common.retry')}
            </button>
          )}
          {job.readyUrl ? (
            <a className="btn btn-primary" href={job.readyUrl} target="_blank" rel="noopener noreferrer" onClick={() => setJob(null)}>
              {t('code.openNow')}
            </a>
          ) : (
            <button className={'btn' + (failed ? ' btn-primary' : '')} onClick={close}>
              {failed ? t('common.close') : t('code.stopWaiting')}
            </button>
          )}
        </div>
      </Dialog>
    )
  }

  return { open, stop: stopCode, running, refresh, dialog }
}
