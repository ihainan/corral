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

// (A reading per host shared by the cards shown within half a minute; the
// gateway keeps its own for five. The host's page reads afresh. review535-4)
const reads = new Map<number, { at: number; p: Promise<AskHookInfo> }>()
const readFor = 30_000

export function readAskHook(hostId: number, fresh = false): Promise<AskHookInfo> {
  const r = reads.get(hostId)
  if (r && !fresh && Date.now() - r.at < readFor) return r.p
  const p = api<AskHookInfo>('GET', `/api/hosts/${hostId}/ask-hook` + (fresh ? '?fresh=1' : ''))
  p.catch(() => reads.delete(hostId))
  reads.set(hostId, { at: Date.now(), p })
  return p
}

function useAskHook(hostId: number, fresh = false) {
  const [info, setInfo] = useState<AskHookInfo | null>(null)
  const [failed, setFailed] = useState<ApiError | Error | null>(null)
  useEffect(() => {
    let live = true
    readAskHook(hostId, fresh).then(
      (i) => live && setInfo(i),
      (e) => live && setFailed(e),
    )
    return () => {
      live = false
    }
  }, [hostId, fresh])
  const set = (i: AskHookInfo) => {
    reads.set(hostId, { at: Date.now(), p: Promise.resolve(i) })
    setInfo(i)
    setFailed(null)
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
  // (The frame's own last line, as the last line: words in a command a
  // permission prompt shows do not make it one — review535-7.)
  const last = o.screen.split('\n').filter((l) => l.trim()).pop() ?? ''
  return o.drawn || /^\s*Enter to select\b.*\bEsc to cancel\s*$/.test(last)
}

const cannotText = (c: NonNullable<AskHookInfo['cannot']>) => t(('askHook.cannot.' + c) as I18nKey)

// AskHookConfirm: turning it on or off, the file and what goes in shown.
// The file is written only if it is still as it was read (seen); else it is
// read again and shown, to be confirmed again.
// onInfo: what the file says now, read again after a refusal or a doubt
// (the page and this dialog then show that — review535-5); onDone only once
// the file is read back as asked (review535-6).
function AskHookConfirm({ hostId, hostName, info, on, onDone, onInfo, onClose }: { hostId: number; hostName: string; info: AskHookInfo; on: boolean; onDone: (i: AskHookInfo) => void; onInfo: (i: AskHookInfo) => void; onClose: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const close = () => !busy && onClose()
  const again = async () => {
    const fresh = await readAskHook(hostId, true).catch(() => null)
    if (fresh) onInfo(fresh)
    return fresh
  }
  const go = async () => {
    setBusy(true)
    setError('')
    try {
      const i = await api<AskHookInfo>('POST', `/api/hosts/${hostId}/ask-hook`, { on, seen: info.seen })
      // (Said done only once the file reads back as asked: on, or off — not
      // a file that can no longer be read — review537-4.)
      if (on ? i.state !== 'on' : i.state !== 'off' && i.state !== 'on-own') {
        onInfo(i)
        setError(t('askHook.notAsAsked'))
        setBusy(false)
        return
      }
      onDone(i)
      onClose()
    } catch (e) {
      const code = e instanceof ApiError ? e.code : ''
      if (code === 'changed') {
        setError(t((await again()) ? 'askHook.changed' : 'askHook.changedUnread'))
      } else if (code === 'uncertain') {
        void again()
        setError(t('askHook.uncertain'))
      } else if (code === 'cannot') {
        const now = (e as ApiError).extra?.info as AskHookInfo | undefined
        if (now) onInfo(now)
        setError(t('askHook.cannotNow'))
      } else if (code === 'write_failed') {
        setError(t('askHook.failed'))
      } else if (code === 'no_connection') {
        setError(t('askHook.state.noConnection'))
      } else if (code === 'busy') {
        setError(t('askHook.busy'))
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
      <p className="hint">{t('askHook.confirmRace')}</p>
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
      onInfo={set}
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
  const { info, failed, set } = useAskHook(hostId, true)
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
          onInfo={set}
          onClose={() => setConfirming(null)}
        />
      )}
    </section>
  )
}
