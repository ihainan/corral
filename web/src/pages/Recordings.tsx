import { useEffect, useMemo, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { ChevronLeft, Download, Film, MoreHorizontal, Pause, Play, Trash2 } from 'lucide-react'
import { api, errorText, type Recording } from '../api'
import { relTime, t } from '../i18n'
import { linkHandler, navigate } from '../router'
import { useSession } from '../app'
import { termScheme } from '../prefs'
import { chromeVars } from '../terminal/themes'
import { formatSize } from '../terminal/FilesDialog'
import { Confirm, EmptyState, LoadError, Menu, useToast } from '../components/ui'

const FONT_STACK = "'JetBrains Mono', 'SFMono-Regular', Menlo, Consolas, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', monospace"

export function formatDuration(secs: number): string {
  const s = Math.max(0, Math.floor(secs))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const pad = (n: number) => String(n).padStart(2, '0')
  return h > 0 ? `${h}:${pad(m)}:${pad(s % 60)}` : `${m}:${pad(s % 60)}`
}

type List = { recordings: Recording[]; enabled: boolean; days: number }

export function RecordingsPage() {
  const { me } = useSession()
  const toast = useToast()
  const [list, setList] = useState<List | null>(null)
  const [failed, setFailed] = useState(false)
  const [more, setMore] = useState(false) // an older page may exist
  const [loadingMore, setLoadingMore] = useState(false)
  const [dialog, setDialog] = useState<React.ReactNode>(null)
  const admin = me?.role === 'admin'
  const PAGE = 100 // as the server sends
  const load = () => {
    setFailed(false)
    api<List>('GET', '/api/recordings')
      .then((l) => {
        setList(l)
        setMore(l.recordings.length === PAGE)
      })
      .catch(() => setFailed(true))
  }
  const loadMore = async () => {
    if (!list || loadingMore) return
    const last = list.recordings[list.recordings.length - 1]
    setLoadingMore(true)
    try {
      const l = await api<List>('GET', `/api/recordings?before=${last.started_at}&after_id=${encodeURIComponent(last.id)}`)
      setList((prev) => {
        if (!prev) return l
        const seen = new Set(prev.recordings.map((r) => r.id))
        return { ...prev, recordings: [...prev.recordings, ...l.recordings.filter((r) => !seen.has(r.id))] }
      })
      setMore(l.recordings.length === PAGE)
    } catch (e) {
      toast(errorText(e), true)
    } finally {
      setLoadingMore(false)
    }
  }
  useEffect(load, [])
  return (
    <main className="page">
      <div className="page-head">
        <h1>{t('rec.title')}</h1>
      </div>
      {list && (
        <p className="lead page-lead">
          {list.enabled ? t('rec.leadOn', { days: list.days }) : t('rec.leadOff')}
          {list.enabled && (admin ? t('rec.leadAdmin') : t('rec.leadMember'))}
        </p>
      )}
      {failed ? (
        <LoadError onRetry={load} />
      ) : list === null ? (
        <p className="loading">{t('common.loading')}</p>
      ) : list.recordings.length === 0 ? (
        <EmptyState icon={<Film size={20} strokeWidth={1.75} />} title={t('rec.empty')} />
      ) : (
        <ul className="rows">
          {list.recordings.map((r) => {
            const live = r.ended_at === 0
            const dur = (live ? Date.now() / 1000 : r.ended_at) - r.started_at
            const href = '/recordings/' + r.id
            return (
              <li className="row" key={r.id}>
                <a className="row-main" href={href} onClick={linkHandler(href)}>
                  <Film className="row-icon" size={18} strokeWidth={1.75} />
                  <span className="row-title">{r.host_name}</span>
                  <span className="row-meta" title={new Date(r.started_at * 1000).toLocaleString()}>{relTime(r.started_at)}</span>
                  <span className="row-sub">
                    {admin && <span className="mono">{r.owner_name}</span>}
                    {admin && ' · '}
                    {live ? t('rec.live') : formatDuration(dur)}
                    {!live && r.bytes >= 0 && ' · ' + formatSize(r.bytes)}
                    {r.truncated && <span className="warn-text">{' · ' + t('rec.truncated')}</span>}
                    {r.gaps && <span className="warn-text">{' · ' + t('rec.gaps')}</span>}
                  </span>
                </a>
                {admin && (
                  <div className="row-actions">
                    <Menu
                      label={r.host_name}
                      button={(open, toggle) => (
                        <button className="icon-btn" aria-label={`${t('common.more')}：${r.host_name}`} aria-expanded={open} onClick={toggle}>
                          <MoreHorizontal size={18} />
                        </button>
                      )}
                      items={() => [
                        { label: t('rec.download'), icon: <Download size={16} />, href: `/api/recordings/${r.id}/cast?download=1` },
                        live
                          ? null
                          : {
                              label: t('common.delete'),
                              icon: <Trash2 size={16} />,
                              danger: true,
                              separator: true,
                              onSelect: () =>
                                setDialog(
                                  <Confirm
                                    title={t('rec.deleteTitle')}
                                    body={t('rec.deleteBody', { host: r.host_name, who: r.owner_name })}
                                    confirm={t('common.delete')}
                                    danger
                                    onClose={() => setDialog(null)}
                                    onConfirm={async () => {
                                      await api('DELETE', '/api/recordings/' + r.id)
                                      toast(t('rec.deleted'))
                                      load()
                                    }}
                                  />,
                                ),
                            },
                      ]}
                    />
                  </div>
                )}
              </li>
            )
          })}
        </ul>
      )}
      {list && more && (
        <div className="load-more">
          <button className="btn" onClick={loadMore} disabled={loadingMore}>{loadingMore ? t('common.loading') : t('rec.more')}</button>
        </div>
      )}
      {dialog}
    </main>
  )
}

// A parsed recording: output and resizes with their times, in seconds.
type Ev = { t: number; kind: 'o' | 'r' | 'm'; data: string }
type Cast = { width: number; height: number; events: Ev[] }

export function parseCast(text: string): Cast {
  const lines = text.split('\n')
  const head = JSON.parse(lines[0] || '{}')
  const events: Ev[] = []
  for (let i = 1; i < lines.length; i++) {
    if (!lines[i]) continue
    try {
      const [time, kind, data] = JSON.parse(lines[i])
      if (typeof time === 'number' && (kind === 'o' || kind === 'r' || kind === 'm') && typeof data === 'string') events.push({ t: time, kind, data })
    } catch {
      break // a recording cut while it was written ends with a partial line
    }
  }
  return { width: Number(head.width) || 80, height: Number(head.height) || 24, events }
}

// Pauses longer than this play as this long, so idle stretches go by quickly.
const IDLE_CAP = 2

// Times as played: each pause capped at IDLE_CAP when skipping idle time.
function playTimes(events: Ev[], skipIdle: boolean): number[] {
  const out: number[] = []
  let last = 0
  let at = 0
  for (const e of events) {
    const gap = Math.max(0, e.t - last)
    at += skipIdle ? Math.min(gap, IDLE_CAP) : gap
    last = e.t
    out.push(at)
  }
  return out
}

const SPEEDS = [0.5, 1, 2, 4, 8]
const WRITE_CHUNK = 64 << 10 // output is fed to the terminal in pieces, each after the last is parsed

// convertPos maps a play position from one time scale to another, keeping
// the same moment of the recording (also between two events).
function convertPos(pos: number, events: Ev[], from: number[], to: number[]): number {
  if (events.length === 0) return 0
  // The same fraction of the way between the events around pos.
  const between = (a0: number, a1: number, b0: number, b1: number) => (a1 > a0 ? b0 + ((pos - a0) / (a1 - a0)) * (b1 - b0) : b0)
  if (pos <= from[0]) return between(0, from[0], 0, to[0])
  let k = 0
  while (k + 1 < from.length && from[k + 1] <= pos) k++
  if (k + 1 >= from.length) return to[to.length - 1]
  return between(from[k], from[k + 1], to[k], to[k + 1])
}

// A terminal fed in order: output is written piece by piece, each piece
// only after the previous one was parsed, and a resize only after the output
// before it, so nothing is laid out at the wrong size.
class Screen {
  term: Terminal
  private chain: Promise<void> = Promise.resolve()
  private dead = false
  constructor(el: HTMLElement, cols: number, rows: number, theme: Terminal['options']['theme'], onResize: () => void) {
    this.term = new Terminal({
      cols, rows, fontFamily: FONT_STACK, fontSize: 13, lineHeight: 1.15, scrollback: 0, disableStdin: true,
      cursorBlink: false, theme, allowProposedApi: true, screenReaderMode: true,
    })
    this.term.loadAddon(new Unicode11Addon())
    this.term.unicode.activeVersion = '11'
    this.term.open(el)
    this.term.onResize(() => requestAnimationFrame(onResize))
  }
  private then(f: () => Promise<void> | void) {
    this.chain = this.chain.then(() => (this.dead ? undefined : f()))
  }
  write(data: string) {
    for (let i = 0; i < data.length; i += WRITE_CHUNK) {
      const part = data.slice(i, i + WRITE_CHUNK)
      this.then(() => new Promise<void>((done) => this.term.write(part, done)))
    }
  }
  resize(cols: number, rows: number) {
    this.then(() => this.term.resize(cols, rows))
  }
  dispose() {
    this.dead = true
    this.term.dispose()
  }
}

export function RecordingPlayerPage({ id }: { id: string }) {
  const [meta, setMeta] = useState<Recording | null>(null)
  const [cast, setCast] = useState<Cast | null>(null)
  const [error, setError] = useState('')
  const [playing, setPlaying] = useState(false)
  const [pos, setPos] = useState(0) // seconds of play time
  const [speed, setSpeed] = useState(1)
  const [skipIdle, setSkipIdle] = useState(true)
  const [screenGen, setScreenGen] = useState(0) // a new terminal for each start from the beginning
  const hostEl = useRef<HTMLDivElement>(null)
  const fitRef = useRef<HTMLDivElement>(null)
  const screen = useRef<Screen | null>(null)
  const shown = useRef(0) // events given to the terminal so far
  const posRef = useRef(0)
  posRef.current = pos
  const scheme = termScheme()

  const loading = useRef<AbortController | null>(null)
  const load = () => {
    setError('')
    loading.current?.abort()
    const abort = new AbortController()
    loading.current = abort
    Promise.all([
      api<Recording>('GET', '/api/recordings/' + id, undefined, { signal: abort.signal }),
      fetch('/api/recordings/' + id + '/cast', { credentials: 'same-origin', signal: abort.signal }).then((r) => {
        if (!r.ok) throw new Error(String(r.status))
        return r.text()
      }),
    ])
      .then(([m, text]) => {
        if (abort.signal.aborted) return
        setMeta(m)
        setCast(parseCast(text))
      })
      .catch((e) => {
        if (abort.signal.aborted) return
        setError(e instanceof Error && /^\d+$/.test(e.message) ? t('rec.loadFailed') : errorText(e))
      })
  }
  useEffect(() => {
    load()
    return () => loading.current?.abort() // also a retry's
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id])
  // A full-screen page like the terminal: the document behind does not scroll.
  useEffect(() => {
    document.documentElement.classList.add('term-mode')
    return () => document.documentElement.classList.remove('term-mode')
  }, [])

  const times = useMemo(() => (cast ? playTimes(cast.events, skipIdle) : []), [cast, skipIdle])
  const total = times.length ? times[times.length - 1] : 0
  const marker = useMemo(() => {
    const ms = cast?.events.filter((e) => e.kind === 'm') ?? []
    return ms.some((m) => m.data === 'truncated') ? 'truncated' : ms.length > 0 ? 'gaps' : ''
  }, [cast])
  const timesRef = useRef(times)
  timesRef.current = times

  // Scaled down as a whole to fit narrower screens (a terminal recorded on
  // a desktop is wide), within the safe area.
  const refit = () => {
    const el = hostEl.current
    const box = fitRef.current
    const xs = el?.querySelector<HTMLElement>('.xterm-screen')
    const stage = box?.parentElement
    if (!el || !box || !xs || !stage) return
    const cs = getComputedStyle(stage)
    const room = stage.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight)
    const w = xs.offsetWidth
    const h = xs.offsetHeight
    const k = w > 0 ? Math.min(1, room / w) : 1
    el.style.transform = k < 1 ? `scale(${k})` : ''
    box.style.width = Math.ceil(w * k) + 'px'
    box.style.height = Math.ceil(h * k) + 'px'
  }
  useEffect(() => {
    if (!cast || !hostEl.current) return
    const sc = new Screen(hostEl.current, cast.width, cast.height, scheme.theme, refit)
    screen.current = sc
    shown.current = 0
    requestAnimationFrame(refit)
    window.addEventListener('resize', refit)
    return () => {
      window.removeEventListener('resize', refit)
      sc.dispose()
      if (screen.current === sc) screen.current = null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cast, screenGen])

  // show brings the terminal to play time `to`: forward by feeding what comes
  // in between; backward by starting a new terminal and feeding from the start.
  const pendingTo = useRef<number | null>(null)
  const show = (to: number) => {
    const sc = screen.current
    const tm = timesRef.current
    if (!cast) return
    if (!sc || (shown.current > 0 && tm[shown.current - 1] > to)) {
      // The old terminal may still be parsing; it is dropped with its queue.
      pendingTo.current = to
      if (sc) {
        screen.current = null
        setScreenGen((g) => g + 1)
      }
      return
    }
    let i = shown.current
    let batch = ''
    for (; i < cast.events.length && tm[i] <= to; i++) {
      const e = cast.events[i]
      if (e.kind === 'o') batch += e.data
      else if (e.kind === 'r') {
        const m = /^(\d+)x(\d+)$/.exec(e.data)
        if (m) {
          if (batch) sc.write(batch)
          batch = ''
          sc.resize(Math.min(1000, Number(m[1])), Math.min(500, Number(m[2])))
        }
      }
    }
    if (batch) sc.write(batch)
    shown.current = i
  }
  // After a new terminal was made for going back, catch it up.
  useEffect(() => {
    if (screen.current && pendingTo.current !== null) {
      const to = pendingTo.current
      pendingTo.current = null
      show(to)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [screenGen, cast])

  // Playback clock: play time runs from where it was at the last play,
  // seek, speed or scale change.
  const clock = useRef({ base: 0, at: 0 })
  useEffect(() => {
    if (!playing) return
    clock.current = { base: posRef.current, at: performance.now() }
    let frame = 0
    const tick = () => {
      const end = timesRef.current.length ? timesRef.current[timesRef.current.length - 1] : 0
      const p = Math.min(end, clock.current.base + ((performance.now() - clock.current.at) / 1000) * speed)
      show(p)
      setPos(p)
      if (p >= end) {
        setPlaying(false)
        return
      }
      frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [playing, speed, skipIdle])

  const seek = (p: number) => {
    show(p)
    setPos(p)
    clock.current = { base: p, at: performance.now() }
  }
  const toggle = () => {
    if (!playing && posRef.current >= total) seek(0)
    setPlaying((v) => !v)
  }
  const toggleRef = useRef(toggle)
  toggleRef.current = toggle
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const tgt = e.target
      if (e.key !== ' ' || tgt instanceof HTMLInputElement || tgt instanceof HTMLSelectElement || tgt instanceof HTMLButtonElement || tgt instanceof HTMLAnchorElement) return
      e.preventDefault()
      if (!e.repeat) toggleRef.current()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  const changeScale = (next: boolean) => {
    if (!cast) return
    const p = convertPos(posRef.current, cast.events, times, playTimes(cast.events, next))
    setSkipIdle(next)
    setPos(p)
    clock.current = { base: p, at: performance.now() }
  }
  // On the new scale the screen is brought to the converted position (the
  // same moment; this only matters at the edges of rounding).
  useEffect(() => {
    if (!playing) show(posRef.current)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [skipIdle])

  return (
    <div className={'term-root player-root ' + (scheme.dark ? 'dark' : 'light')} style={chromeVars(scheme) as React.CSSProperties}>
      <header className="term-bar">
        <button className="icon-btn" onClick={() => navigate('/recordings')} aria-label={t('common.back')} title={t('common.back')}>
          <ChevronLeft size={20} />
        </button>
        <div className="player-title">
          <span className="term-host-name">{meta?.host_name ?? ''}</span>
          {meta && (
            <span className="term-status">
              {meta.owner_name} · {new Date(meta.started_at * 1000).toLocaleString()}
              {meta.ended_at === 0 ? ' · ' + t('rec.live') : ''}
            </span>
          )}
        </div>
        <div className="spacer" />
        <a className="icon-btn" href={`/api/recordings/${id}/cast?download=1`} aria-label={t('rec.download')} title={t('rec.download')}>
          <Download size={19} />
        </a>
      </header>
      <div className="player-stage">
        {error ? (
          <LoadError text={error} onRetry={() => load()} />
        ) : !cast ? (
          <p className="hint">{t('common.loading')}</p>
        ) : (
          <div className="player-fit" ref={fitRef}>
            <div className="player-screen" ref={hostEl} key={screenGen} onClick={toggle} />
          </div>
        )}
        {marker && <p className="hint player-note">{marker === 'truncated' ? t('rec.truncatedNote') : t('rec.gapsNote')}</p>}
      </div>
      <div className="player-controls">
        <button className="icon-btn player-play" onClick={toggle} disabled={!cast} aria-label={playing ? t('rec.pause') : t('rec.play')} title={(playing ? t('rec.pause') : t('rec.play')) + ' (Space)'}>
          {playing ? <Pause size={20} /> : <Play size={20} />}
        </button>
        <span className="tnum player-time">{formatDuration(pos)} / {formatDuration(total)}</span>
        <input
          className="player-seek"
          type="range"
          min={0}
          max={Math.max(total, 0.001)}
          step="any" // any position: a step would keep the slider off the very end
          value={pos}
          disabled={!cast}
          aria-label={t('rec.position')}
          aria-valuetext={formatDuration(pos)}
          onChange={(e) => seek(Number(e.target.value))}
        />
        <select className="player-speed" value={speed} onChange={(e) => setSpeed(Number(e.target.value))} aria-label={t('rec.speed')}>
          {SPEEDS.map((s) => <option key={s} value={s}>{s}×</option>)}
        </select>
        <label className="player-idle">
          <input type="checkbox" checked={skipIdle} onChange={(e) => changeScale(e.target.checked)} />
          {t('rec.skipIdle')}
        </label>
      </div>
    </div>
  )
}
