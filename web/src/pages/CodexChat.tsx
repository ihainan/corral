import { memo, useCallback, useEffect, useRef, useState } from 'react'
import { ArrowLeft, ArrowUp, Paperclip, Plus, RefreshCw, Square, X } from 'lucide-react'
import { api, errorText, uploadFile, type SessionInfo } from '../api'
import { useSession } from '../app'
import { t } from '../i18n'
import { navigate } from '../router'
import { copyText, useToast } from '../components/ui'
import { Markdown } from '../terminal/markdown'
import { thoughtTitle } from '../terminal/Conversation'
import { getDraft, keepDraft } from '../terminal/drafts'
import { local } from '../storage'
import { isDark, usePrefs } from '../prefs'
import { applyNativeEvent, importedTool, nativeUserText, type NativeEvent, type NativeItem, type NativeRow, type NativeSnapshot, type NativeThread } from '../terminal/codexNative'

interface Model { id: string; model: string; displayName: string; supportedReasoningEfforts: { reasoningEffort: string }[] }
interface Attachment { name: string; path: string }
const address = (connection: string, thread = '') => '/codex/' + connection + (thread ? '/' + thread : '')

export function CodexChat({ connection = '', thread = '' }: { connection?: string; thread?: string }) {
  const { me } = useSession()
  const toast = useToast()
  usePrefs()
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [threads, setThreads] = useState<NativeThread[]>([])
  const [listCursor, setListCursor] = useState<string | null>(null)
  const [snapshot, setSnapshot] = useState<NativeSnapshot | null>(null)
  const [rows, setRows] = useState<NativeRow[]>([])
  const [requests, setRequests] = useState<NativeEvent[]>([])
  const [models, setModels] = useState<Model[]>([])
  const [busy, setBusy] = useState(false)
  const locked = useRef(false)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [live, setLive] = useState(false)
  const [wire, setWire] = useState(0)
  const [cwd, setCwd] = useState('')
  const [creating, setCreating] = useState(false)
  const [attachments, setAttachments] = useState<Attachment[]>([])
  const selected = connection + '/' + thread
  const selection = useRef(selected)
  selection.current = selected
  const draftKey = 'codex ' + (sessions.find(s => s.id === connection)?.host_id ?? connection) + ' ' + thread
  const [draft, setDraft] = useState(() => getDraft(draftKey))
  const scroll = useRef<HTMLDivElement>(null)
  const following = useRef(true)
  const images = useRef<HTMLInputElement>(null)
  const base = '/api/sessions/' + connection + '/codex'

  useEffect(() => {
    const ctl = new AbortController()
    api<SessionInfo[]>('GET', '/api/sessions', undefined, { signal: ctl.signal }).then(list => {
      setSessions(list.filter(s => s.state === 'running'))
    }).catch(e => !ctl.signal.aborted && setError(errorText(e)))
    return () => ctl.abort()
  }, [])

  const listThreads = useCallback(async (cursor?: string) => {
    if (!connection) return
    const at = connection
    try {
      const result = await api<{ data: NativeThread[]; nextCursor: string | null }>('GET', base + '/threads' + (cursor ? '?cursor=' + encodeURIComponent(cursor) : ''))
      if (!selection.current.startsWith(at + '/')) return
      setThreads(prev => cursor ? [...prev, ...result.data.filter(r => !prev.some(p => p.id === r.id))] : result.data)
      setListCursor(result.nextCursor)
    } catch (e) { if (selection.current.startsWith(at + '/')) setError(errorText(e)) }
  }, [base, connection])

  const refresh = useCallback(async (cursor?: string) => {
    if (!thread || !connection) return
    const at = selected
    setLoading(true)
    try {
      const result = await api<NativeSnapshot>('GET', base + '/threads/' + thread + (cursor ? '?cursor=' + encodeURIComponent(cursor) : ''))
      if (selection.current !== at) return
      const loaded = result.page.data.map(r => ({ ...r, item: { ...r.item, _history: r.completedAtMs != null } })).reverse()
      setRows(prev => cursor ? [...loaded.filter(r => !prev.some(p => p.item.id === r.item.id)), ...prev].slice(-1000) : loaded)
      setSnapshot(result)
      setRequests(result.requests ?? [])
      setError('')
      if (!cursor) setWire(n => n + 1)
    } catch (e) { if (selection.current === at) setError(errorText(e)) }
    finally { if (selection.current === at) setLoading(false) }
  }, [base, connection, thread, selected])

  useEffect(() => {
    setThreads([]); setListCursor(null); setModels([]); setError('')
    if (!connection) return
    void listThreads()
    const ctl = new AbortController()
    api<{ data: Model[] }>('GET', base + '/models', undefined, { signal: ctl.signal }).then(r => setModels(r.data)).catch(() => {})
    return () => ctl.abort()
  }, [connection, base, listThreads])

  useEffect(() => {
    setSnapshot(null); setRows([]); setRequests([]); setAttachments([]); setLive(false); setBusy(false); locked.current = false
    setDraft(getDraft(draftKey)); following.current = true
    if (thread) void refresh()
  }, [thread, connection, draftKey, refresh])

  useEffect(() => {
    if (!thread || !snapshot?.connected) return
    const source = new EventSource(base + '/threads/' + thread + '/events?after=' + snapshot.seq)
    const queued: NativeEvent[] = []
    let frame = 0
    let retry = 0
    const flush = () => { frame = 0; const batch = queued.splice(0); setRows(prev => batch.reduce(applyNativeEvent, prev)) }
    source.onmessage = event => {
      const e = JSON.parse(event.data) as NativeEvent
      if (e.method === 'ready') { setLive(true); setRequests(e.requests ?? []); return }
      if (e.method === 'heartbeat') return
      if (e.method === 'resync') { source.close(); void refresh(); return }
      if (e.thread !== thread) return
      if (e.method === 'error') { setError(t('native.taskError')); return }
      if (e.request_id !== undefined) setRequests(prev => [...prev.filter(p => p.request_id !== e.request_id), e])
      if (e.method === 'serverRequest/resolved') setRequests(prev => prev.filter(p => p.request_id !== e.params?.requestId))
      if (e.method === 'turn/started') {
        const turn = e.params?.turn as { id: string }
        setSnapshot(prev => prev && ({ ...prev, turn: turn.id, thread: { ...prev.thread, status: { type: 'active' } } }))
      }
      if (e.method === 'turn/completed') {
        setSnapshot(prev => prev && ({ ...prev, turn: '', thread: { ...prev.thread, status: { type: 'idle' } } }))
      }
      if (e.method === 'thread/status/changed') setSnapshot(prev => prev && ({ ...prev, thread: { ...prev.thread, status: e.params?.status as NativeThread['status'] } }))
      queued.push(e)
      if (!frame) frame = requestAnimationFrame(flush)
    }
    source.onerror = () => { source.close(); setLive(false); retry = window.setTimeout(() => { if (!document.hidden) void refresh() }, 2000) }
    const visible = () => {
      if (document.hidden) { source.close(); setLive(false) }
      else void refresh()
    }
    document.addEventListener('visibilitychange', visible)
    return () => { source.close(); cancelAnimationFrame(frame); clearTimeout(retry); document.removeEventListener('visibilitychange', visible) }
    // Snapshot refresh advances the replay cursor; ordinary events do not reopen the stream.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [base, thread, wire, snapshot?.connected])

  useEffect(() => {
    if (following.current) scroll.current?.scrollTo({ top: scroll.current.scrollHeight })
  }, [rows, requests])

  const command = async (action: string, body: Record<string, unknown> = {}, key: string = crypto.randomUUID()) => {
    const at = selected
    setBusy(true)
    try {
      const result = await api<{ thread?: NativeThread; turn?: { id: string } }>('POST', base + '/threads' + (action === 'create' ? '' : '/' + thread), { action, key, ...body }, { timeoutMs: 25000 })
      if (selection.current !== at) return null
      if (action === 'create' && result.thread) { setCreating(false); navigate(address(connection, result.thread.id)); return result }
      if (action !== 'send') await refresh()
      return result
    } catch (e) { if (selection.current === at) { setError(errorText(e)); toast(errorText(e), true) }; return null }
    finally { if (selection.current === at) setBusy(false) }
  }

  const send = async () => {
    if (locked.current || busy || !live || (!draft.trim() && !attachments.length)) return
    locked.current = true
    setBusy(true)
    const text = draft, files = attachments.map(a => a.path), at = selected, savedDraft = draftKey
    try {
    const sum = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(JSON.stringify({ text, files })))
    if (selection.current !== at) return
    const hash = [...new Uint8Array(sum)].map(n => n.toString(16).padStart(2, '0')).join('')
    const keyPath = 'corral.codex.command.' + me?.id + '.' + savedDraft
    const saved = local.get(keyPath)?.split(':')
    const key = saved?.[0] === hash ? saved[1] : crypto.randomUUID()
    local.set(keyPath, hash + ':' + key)
    const result = await command('send', { text, images: files, turn: snapshot?.turn ?? '' }, key)
    if (result) {
      local.remove(keyPath)
      if (getDraft(savedDraft) === text) keepDraft(savedDraft, '')
      if (selection.current === at) { setDraft(''); setAttachments([]); following.current = true }
    }
    } catch (e) { if (selection.current === at) setError(errorText(e)) }
    finally { if (selection.current === at) { locked.current = false; setBusy(false) } }
  }

  const attach = async (files: FileList | null) => {
    if (!files) return
    const at = selected; setBusy(true)
    try {
      const picked = [...files].slice(0, 8 - attachments.length)
      if (picked.some(file => !file.type.startsWith('image/'))) throw new Error(t('native.imagesOnly'))
      const uploaded: Attachment[] = []
      for (const file of picked) { const res = await uploadFile(connection, file); uploaded.push({ name: file.name, path: res.path }) }
      if (selection.current === at) setAttachments(prev => [...prev, ...uploaded])
    } catch (e) { toast(errorText(e), true) }
    finally { if (selection.current === at) setBusy(false) }
  }

  const model = models.find(m => m.model === snapshot?.thread.model)
  const connected = !!snapshot?.connected
  return (
    <div className={'native-page term-vars ' + (isDark() ? 'dark' : 'light')}>
      <header className="native-header">
        <button className="icon-btn" onClick={() => navigate(thread ? address(connection) : '/agents')} aria-label={t('common.back')}><ArrowLeft size={18} /></button>
        <h1>Codex</h1>
        <select aria-label={t('native.connection')} value={connection} onChange={e => navigate(address(e.target.value))}>
          <option value="">{t('native.chooseConnection')}</option>
          {sessions.map(s => <option key={s.id} value={s.id}>{s.host_name}{s.mux_session ? ' · ' + s.mux_session : ''}</option>)}
        </select>
        <button className="icon-btn" disabled={!connection || busy} onClick={() => { setCreating(true); setCwd(snapshot?.thread.cwd ?? '') }} aria-label={t('native.new')}><Plus size={18} /></button>
        <button className="icon-btn" disabled={!connection || loading} onClick={() => { void listThreads(); void refresh() }} aria-label={t('common.retry')}><RefreshCw size={16} /></button>
      </header>
      {creating && <form className="native-create" onSubmit={e => { e.preventDefault(); void command('create', { cwd }) }}>
        <label>{t('native.folder')}<input value={cwd} onChange={e => setCwd(e.target.value)} placeholder="/path/to/project" required /></label>
        <button className="btn btn-primary" disabled={busy}>{t('native.new')}</button><button type="button" className="btn" onClick={() => setCreating(false)}>{t('common.cancel')}</button>
      </form>}
      {error && <p className="native-error" role="alert">{error}</p>}
      <div className={'native-layout' + (thread ? ' has-thread' : '')}>
        <aside className="native-list" aria-label={t('native.conversations')}>
          {threads.map(item => <button key={item.id} className={'native-list-item' + (item.id === thread ? ' is-current' : '')} onClick={() => navigate(address(connection, item.id))} title={item.cwd}>
            <strong>{item.name || nativeUserText(item.preview ?? '').split('\n')[0] || t('native.untitled')}</strong><small>{item.cwd}</small>
          </button>)}
          {listCursor && <button className="btn" onClick={() => void listThreads(listCursor)}>{t('common.more')}</button>}
          {!connection && <p className="hint">{t('native.chooseConnection')}</p>}
          {!sessions.length && <p className="hint">{t('native.noHosts')}</p>}
          {connection && !threads.length && !error && <p className="hint">{t('native.empty')}</p>}
        </aside>
        <section className="native-chat">
          {snapshot && <div className="native-thread-bar"><strong>{snapshot.thread.name || t('native.untitled')}</strong><span className="hint">{live ? t('native.live') : t('native.readOnly')}</span>
            <select aria-label={t('native.model')} value={snapshot.thread.model ?? ''} disabled={!connected || busy} onChange={e => void command('settings', { model: e.target.value })}>
              {!model && <option value={snapshot.thread.model ?? ''}>{snapshot.thread.model ?? t('native.default')}</option>}{models.map(m => <option key={m.id} value={m.model}>{m.displayName}</option>)}
            </select>
            <select aria-label={t('native.effort')} value={snapshot.thread.reasoningEffort ?? ''} disabled={!connected || busy || !model} onChange={e => void command('settings', { effort: e.target.value, reset_effort: !e.target.value })}>
              <option value="">{t('native.default')}</option>{model?.supportedReasoningEfforts.map(e => <option key={e.reasoningEffort}>{e.reasoningEffort}</option>)}
            </select>
          </div>}
          <div className="native-messages" ref={scroll} onScroll={() => { const el = scroll.current; if (el) following.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80 }}>
            {loading && !rows.length && <p className="hint">{t('common.loading')}</p>}
            {snapshot?.page.nextCursor && <button className="btn" disabled={loading} onClick={() => void refresh(snapshot.page.nextCursor!)}>{t('native.older')}</button>}
            {!thread && <p className="hint">{t('native.chooseThread')}</p>}
            <NativeTimeline rows={rows} />
            {requests.map(request => <NativeRequest key={String(request.request_id)} request={request} busy={busy} reply={body => command('reply', { request: request.request_id, ...body })} />)}
          </div>
          {snapshot && (!connected || !live) && <div className="native-connect"><p>{t(connected ? 'native.disconnected' : 'native.connectHint')}</p><button className="btn btn-primary" disabled={busy || snapshot.thread.canAcceptDirectInput === false} onClick={() => void command('connect')}>{t('native.connect')}</button></div>}
          {snapshot && <form className="native-composer" onSubmit={e => { e.preventDefault(); void send() }}>
            {!!attachments.length && <div className="native-attachments">{attachments.map((a, i) => <button type="button" key={a.path} onClick={() => setAttachments(prev => prev.filter((_, n) => n !== i))}>{a.name}<X size={14} /></button>)}</div>}
            <textarea aria-label={t('term.composer')} placeholder={t('native.message')} value={draft} onChange={e => { setDraft(e.target.value); keepDraft(draftKey, e.target.value) }} rows={3}
              onKeyDown={e => { if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); void send() } }} />
            <div className="native-composer-actions"><input ref={images} type="file" accept="image/*" multiple hidden onChange={e => { void attach(e.target.files); e.target.value = '' }} />
              <button type="button" className="icon-btn" disabled={busy || !live} onClick={() => images.current?.click()} aria-label={t('native.attach')}><Paperclip size={18} /></button>
              {snapshot.turn && <button type="button" className="icon-btn" disabled={busy || !live} onClick={() => void command('interrupt', { turn: snapshot.turn })} aria-label={t('convo.stop')}><Square size={16} /></button>}
              <span className="hint">{snapshot.turn ? t('native.steer') : ''}</span>
              <button type="submit" className="icon-btn" disabled={!live || busy || (!draft.trim() && !attachments.length)} aria-label={t('term.send')}><ArrowUp size={20} /></button>
            </div>
          </form>}
        </section>
      </div>
    </div>
  )
}

function NativeTimeline({ rows }: { rows: NativeRow[] }) {
  const groups: { key: string; rows: NativeRow[]; steps: boolean }[] = []
  for (const row of rows) {
    const steps = !['userMessage', 'agentMessage'].includes(row.item.type) || !!importedTool(row.item.text ?? '')
    const prev = groups[groups.length - 1]
    if (steps && prev?.steps) prev.rows.push(row)
    else groups.push({ key: row.item.id, rows: [row], steps })
  }
  return <>{groups.map(group => group.steps
    ? <details className="native-steps" key={group.key}><summary>{t('native.steps', { n: group.rows.length })}</summary>{group.rows.map(row => <NativeItemView key={row.item.id} item={row.item} />)}</details>
    : <NativeItemView key={group.key} item={group.rows[0].item} />)}</>
}

const NativeItemView = memo(function NativeItemView({ item }: { item: NativeItem }) {
  const toast = useToast()
  const copyCode = (text: string) => { void copyText(text).then(() => toast(t('common.copied'))).catch(e => toast(errorText(e), true)) }
  if (item.type === 'userMessage') return <div className="native-user">{item.content?.map((part, i) => part.text ? <div key={i}>{nativeUserText(part.text)}</div> : <span key={i}>{part.path?.split('/').pop() ?? t('native.image')}</span>)}</div>
  if (item.type === 'agentMessage') {
    const tool = importedTool(item.text ?? '')
    return tool ? <details className="native-tool"><summary>{tool.title}</summary><pre>{tool.body}</pre></details> : <div className="native-answer convo-md"><Markdown text={item.text ?? ''} onCopyCode={copyCode} copyLabel={t('common.copy')} /></div>
  }
  if (item.type === 'reasoning') {
    const text = item.summary?.join('\n\n') ?? item.text ?? ''
    return <details className="native-thought"><summary>{thoughtTitle(text) || t('convo.thinking')}</summary><Markdown text={text} /></details>
  }
  const command = Array.isArray(item.command) ? item.command.join(' ') : item.command
  const title = command?.split('\n')[0] || item.tool || item.name || (item.type === 'fileChange' ? t('native.filesChanged') : item.type)
  return <details className="native-tool"><summary>{title}{item.status === 'failed' && <span> · {t('native.failed')}</span>}</summary>
    {command && <pre>{command}</pre>}{item.aggregatedOutput && <pre>{item.aggregatedOutput.slice(-65536)}</pre>}
    {item.changes?.map(change => <div key={change.path}><strong>{change.path}</strong><pre>{change.diff?.slice(0, 65536)}</pre></div>)}
    {item.result !== undefined && <pre>{JSON.stringify(item.result, null, 2).slice(0, 65536)}</pre>}
  </details>
})

function NativeRequest({ request, busy, reply }: { request: NativeEvent; busy: boolean; reply: (body: Record<string, unknown>) => Promise<unknown> }) {
  const [answers, setAnswers] = useState<Record<string, string>>({})
  const questions = request.params?.questions as { id: string; question: string; options?: { label: string; description: string }[] }[] | undefined
  const ask = request.method === 'item/tool/requestUserInput'
  const decisions = request.params?.availableDecisions as string[] | undefined
  const canAccept = request.method === 'item/commandExecution/requestApproval' && typeof request.params?.command === 'string' && !request.params?.additionalPermissions && (!decisions || decisions.includes('accept'))
  const command = request.params?.command
  const reason = request.params?.reason
  return <div className="native-request" role="group" aria-label={t('native.approval')}>
    {ask ? questions?.map(q => <label key={q.id}>{q.question}{q.options && <small>{q.options.map(o => o.label + ' — ' + o.description).join('\n')}</small>}<input value={answers[q.id] ?? ''} onChange={e => setAnswers(prev => ({ ...prev, [q.id]: e.target.value }))} /></label>)
      : <><strong>{t('native.approval')}</strong>{typeof reason === 'string' && <p>{reason}</p>}{typeof command === 'string' && <pre>{command}</pre>}{request.method === 'item/permissions/requestApproval' && <p>{t('native.permissions')}</p>}{request.method === 'mcpServer/elicitation/request' && <p>{t('native.elicitation')}</p>}</>}
    {ask ? <button className="btn btn-primary" disabled={busy || questions?.some(q => !answers[q.id]?.trim())} onClick={() => void reply({ answers: Object.fromEntries(Object.entries(answers).map(([id, answer]) => [id, [answer]])) })}>{t('term.send')}</button>
      : <div className="native-request-actions">{canAccept && <button className="btn btn-primary" disabled={busy} onClick={() => void reply({ decision: 'accept' })}>{t('native.allowOnce')}</button>}<button className="btn" disabled={busy} onClick={() => void reply({ decision: 'decline' })}>{t('native.decline')}</button></div>}
  </div>
}
