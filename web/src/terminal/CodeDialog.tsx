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
}

// useCode gives the "Open in VS Code" and "Stop VS Code" actions, the
// progress dialog to render, and which hosts are known to run VS Code.
export function useCode() {
  const toast = useToast()
  const [job, setJob] = useState<Job | null>(null)
  const [running, setRunning] = useState<Record<number, boolean>>({})
  const cancelled = useRef<(() => void) | null>(null)

  useEffect(() => () => cancelled.current?.(), [])

  const markRunning = (hostId: number, on: boolean) => setRunning((r) => ({ ...r, [hostId]: on }))

  // refresh asks the server which of these hosts run VS Code.
  const refresh = useCallback((hostIds: number[]) => {
    for (const id of hostIds) {
      api<CodeStatus>('GET', `/api/hosts/${id}/code`)
        .then((st) => markRunning(id, st.state === 'ready'))
        .catch(() => {})
    }
  }, [])

  const open = (hostId: number, name: string) => {
    cancelled.current?.()
    const win = window.open('about:blank', '_blank')
    // VS Code must not be able to navigate this tab.
    if (win) win.opener = null
    let stop = false
    cancelled.current = () => {
      stop = true
      win?.close()
    }
    setJob({ hostId, name, status: { state: 'checking' } })
    const fail = (failure: string, status: CodeStatus = { state: 'failed' }) => {
      win?.close()
      if (!stop) setJob({ hostId, name, status, failure })
      cancelled.current = null
    }
    const poll = async (first: CodeStatus) => {
      let st = first
      while (!stop && working(st.state)) {
        setJob({ hostId, name, status: st })
        await new Promise((r) => setTimeout(r, 800))
        if (stop) return
        st = await api<CodeStatus>('GET', `/api/hosts/${hostId}/code`)
      }
      if (stop) return
      if (st.state === 'ready' && st.url) {
        markRunning(hostId, true)
        if (win) win.location.href = st.url
        else window.location.href = st.url
        cancelled.current = null
        setJob(null)
        return
      }
      fail(failureText(st), st)
    }
    api<CodeStatus>('POST', `/api/hosts/${hostId}/code`)
      .then(poll)
      .catch((e) => fail(errorText(e)))
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
    cancelled.current?.()
    cancelled.current = null
    setJob(null)
  }

  let dialog: ReactNode = null
  if (job) {
    const failed = !!job.failure
    dialog = (
      <Dialog title={t('code.title', { name: job.name })} onClose={close}>
        {failed ? (
          <p className="err" role="alert">{job.failure}</p>
        ) : (
          <p className="lead" role="status" aria-live="polite">{t(('code.state.' + job.status.state) as Parameters<typeof t>[0])}</p>
        )}
        {!failed && (job.status.state === 'downloading' || job.status.state === 'uploading') && <p className="lead">{t('code.firstTime')}</p>}
        <div className="dialog-actions">
          {failed && (
            <button className="btn" onClick={() => open(job.hostId, job.name)}>
              {t('common.retry')}
            </button>
          )}
          <button className={'btn' + (failed ? ' btn-primary' : '')} onClick={close}>
            {failed ? t('common.close') : t('common.cancel')}
          </button>
        </div>
      </Dialog>
    )
  }

  return { open, stop: stopCode, running, refresh, dialog }
}
