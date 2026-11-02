// Claude Code's question hook on a host (internal/askhook, web/askhook.go):
// Claude Code holds a question back from its log till it is answered, so a
// card drawn while it waits has only the screen; with the hook in its
// settings it writes the question at once. Offered on such a card and on the
// host's page; written only once the person confirms, the file named and
// what goes into it shown.
import { useEffect, useState } from 'react'
import { api, ApiError, errorText } from '../api'
import { t, type I18nKey } from '../i18n'
import { Dialog, useToast } from '../components/ui'

export interface AskHookInfo {
  state?: 'on' | 'on-own' | 'off' | 'blocked'
  why?: string
  cannot?: 'link' | 'hard_link' | 'not_file' | 'invalid' | 'no_sum'
  file: string
  seen: string
  snippet: string
  declined: boolean
}

// (One reading per host for the page's life, shared by its cards; read
// again after a change, or when asked to.)
const reads = new Map<number, Promise<AskHookInfo>>()

export function readAskHook(hostId: number, fresh = false): Promise<AskHookInfo> {
  let p = reads.get(hostId)
  if (!p || fresh) {
    p = api<AskHookInfo>('GET', `/api/hosts/${hostId}/ask-hook` + (fresh ? '?fresh=1' : ''))
    p.catch(() => reads.delete(hostId))
    reads.set(hostId, p)
  }
  return p
}

function useAskHook(hostId: number) {
  const [info, setInfo] = useState<AskHookInfo | null>(null)
  const [failed, setFailed] = useState<ApiError | Error | null>(null)
  useEffect(() => {
    let live = true
    readAskHook(hostId).then(
      (i) => live && setInfo(i),
      (e) => live && setFailed(e),
    )
    return () => {
      live = false
    }
  }, [hostId])
  const set = (i: AskHookInfo) => {
    reads.set(hostId, Promise.resolve(i))
    setInfo(i)
  }
  return { info, failed, set }
}

// hintOf: what the card says of the hook, from what the host's file says:
// nothing (on — the question was asked before it — or not read), that hooks
// are turned off there, that the file is one Corral leaves alone, the offer,
// or (declined) only a quiet way back to it.
export function hintOf(info: AskHookInfo | null): 'none' | 'blocked' | 'cannot' | 'offer' | 'declined' {
  if (!info || info.state === 'on' || info.state === 'on-own') return 'none'
  if (info.state === 'blocked') return 'blocked'
  if (info.cannot) return 'cannot'
  if (!info.state) return 'none'
  return info.declined ? 'declined' : 'offer'
}

// asksFromScreen: a question of Claude Code's drawn from its screen, its log
// not having it — where the card may offer the hook: drawn as one (a question
// or a review page read off the screen), or the screen itself shown, with
// Claude Code's question frame's own last line on it.
export function asksFromScreen(o: { agent?: string; logAsks: boolean; screen?: string; drawn: boolean }): boolean {
  if (o.agent !== 'claude' || o.logAsks || o.screen === undefined) return false
  return o.drawn || (/Enter to select/.test(o.screen) && /Esc to cancel/.test(o.screen))
}

const cannotText = (c: NonNullable<AskHookInfo['cannot']>) => t(('askHook.cannot.' + c) as I18nKey)

// AskHookConfirm: turning it on or off, the file and what goes in shown.
// The file is written only if it is still as it was read (seen); else it is
// read again and shown, to be confirmed again.
function AskHookConfirm({ hostId, hostName, info, on, onDone, onClose }: { hostId: number; hostName: string; info: AskHookInfo; on: boolean; onDone: (i: AskHookInfo) => void; onClose: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [seen, setSeen] = useState(info.seen)
  const close = () => !busy && onClose()
  const go = async () => {
    setBusy(true)
    setError('')
    try {
      const i = await api<AskHookInfo>('POST', `/api/hosts/${hostId}/ask-hook`, { on, seen })
      onDone(i)
      onClose()
    } catch (e) {
      if (e instanceof ApiError && e.code === 'changed') {
        const fresh = await readAskHook(hostId, true).catch(() => null)
        if (fresh) setSeen(fresh.seen)
        setError(t('askHook.changed'))
      } else if (e instanceof ApiError && e.code === 'uncertain') {
        void readAskHook(hostId, true).catch(() => null)
        setError(t('askHook.uncertain'))
      } else if (e instanceof ApiError && (e.code === 'write_failed' || e.code === 'cannot')) {
        setError(t('askHook.failed'))
      } else {
        setError(errorText(e))
      }
      setBusy(false)
    }
  }
  return (
    <Dialog title={t(on ? 'askHook.confirmTitle' : 'askHook.offTitle', { host: hostName })} onClose={close}>
      <p className="lead">{t(on ? 'askHook.confirmBody' : 'askHook.offBody', { file: info.file })}</p>
      {on && <pre className="ask-hook-snippet">{info.snippet}</pre>}
      {on && <p className="hint">{t('askHook.confirmRace')}</p>}
      {error && <p className="err" role="alert">{error}</p>}
      <div className="dialog-actions">
        <button className="btn" onClick={close} disabled={busy}>{t('common.cancel')}</button>
        <button className={'btn ' + (on ? 'btn-primary' : 'btn-danger')} disabled={busy} onClick={() => void go()}>
          {t(on ? 'askHook.confirm' : 'askHook.off')}
        </button>
      </div>
    </Dialog>
  )
}

// What to add by hand, where the gateway does not write the file.
function Snippet({ info }: { info: AskHookInfo }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <button type="button" className="link-btn" aria-expanded={open} onClick={() => setOpen(!open)}>
        {t(open ? 'askHook.hideSnippet' : 'askHook.showSnippet')}
      </button>
      {open && <pre className="ask-hook-snippet">{info.snippet}</pre>}
    </>
  )
}

// AskHookHint: on a card of Claude Code's drawn from the screen, why it may
// be short of the question, and the way to have it whole from the next one.
// Nothing where it is on (the question was asked before it), or the host's
// file cannot be read now.
export function AskHookHint({ hostId, hostName }: { hostId: number; hostName: string }) {
  const toast = useToast()
  const { info, set } = useAskHook(hostId)
  const [confirming, setConfirming] = useState(false)
  const kind = hintOf(info)
  if (!info || kind === 'none') return null
  if (kind === 'blocked') return <p className="convo-ask-unread">{t('askHook.blocked', { why: info.why ?? '' })}</p>
  if (info.cannot) {
    return (
      <div className="ask-hook-hint">
        <p className="convo-ask-unread">{t('askHook.cannot', { why: cannotText(info.cannot), file: info.file })}</p>
        <Snippet info={info} />
      </div>
    )
  }
  const decline = () => {
    set({ ...info, declined: true })
    void api('POST', `/api/hosts/${hostId}/ask-hook`, { declined: true }).catch(() => {})
  }
  const confirm = confirming && (
    <AskHookConfirm
      hostId={hostId}
      hostName={hostName}
      info={info}
      on
      onDone={(i) => {
        set(i)
        toast(t('askHook.done'))
      }}
      onClose={() => setConfirming(false)}
    />
  )
  if (kind === 'declined') {
    return (
      <div className="ask-hook-hint">
        <button type="button" className="link-btn" onClick={() => setConfirming(true)}>
          {t('askHook.enableLink')}
        </button>
        {confirm}
      </div>
    )
  }
  return (
    <div className="ask-hook-hint">
      <p className="convo-ask-unread">{t('askHook.why')}</p>
      <div className="convo-ask-more">
        <button type="button" className="btn btn-sm" onClick={() => setConfirming(true)}>
          {t('askHook.enable')}
        </button>
        <button type="button" className="btn btn-ghost btn-sm" onClick={decline}>
          {t('askHook.decline')}
        </button>
      </div>
      {confirm}
    </div>
  )
}

// AskHookRow: the host's page — whether it is on, and the switch.
export function AskHookRow({ hostId, hostName }: { hostId: number; hostName: string }) {
  const toast = useToast()
  const { info, failed, set } = useAskHook(hostId)
  const [confirming, setConfirming] = useState<boolean | null>(null)
  let state: string
  if (failed) state = t(failed instanceof ApiError && failed.code === 'no_connection' ? 'askHook.state.noConnection' : 'askHook.state.unknown')
  else if (!info) state = t('common.loading')
  else if (info.state === 'blocked') state = t('askHook.state.blocked', { why: info.why ?? '' })
  else if (info.state) state = t(('askHook.state.' + info.state) as I18nKey)
  else state = t('askHook.state.unknown')
  const canChange = !!info && !info.cannot && (info.state === 'on' || info.state === 'off')
  return (
    <section className="ask-hook-row" aria-labelledby="ask-hook-title">
      <h3 id="ask-hook-title">{t('askHook.row')}</h3>
      <p className="hint">{t('askHook.rowHint', { file: info?.file ?? '~/.claude/settings.json' })}</p>
      <p className="ask-hook-state" role="status">
        {state}
        {info?.cannot && <> · {cannotText(info.cannot)}</>}
      </p>
      {info?.cannot && <Snippet info={info} />}
      {canChange && (
        <div>
          <button type="button" className="btn" onClick={() => setConfirming(info.state === 'off')}>
            {t(info.state === 'off' ? 'askHook.turnOn' : 'askHook.turnOff')}
          </button>
        </div>
      )}
      {confirming !== null && info && (
        <AskHookConfirm
          hostId={hostId}
          hostName={hostName}
          info={info}
          on={confirming}
          onDone={(i) => {
            set(i)
            toast(t(confirming ? 'askHook.done' : 'askHook.offDone'))
          }}
          onClose={() => setConfirming(null)}
        />
      )}
    </section>
  )
}
