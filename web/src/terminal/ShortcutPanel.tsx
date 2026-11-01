import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { ArrowDown, ArrowLeft, ArrowRight, ArrowUp, ChevronLeft, ChevronRight, Columns2, LayoutGrid, Maximize2, Plus, Rows2, SquareX, X } from 'lucide-react'
import { t } from '../i18n'
import { keysInUse } from '../components/ui'
import { pageCommon, pageGroups, pageTitles, type HerdrAct, type PanelPage, type Shortcut } from './shortcuts'

const icons: Partial<Record<HerdrAct, typeof Plus>> = {
  new_workspace: LayoutGrid, new_tab: Plus, split_right: Columns2, split_down: Rows2, zoom_toggle: Maximize2,
  focus_left: ArrowLeft, focus_right: ArrowRight, focus_up: ArrowUp, focus_down: ArrowDown,
  prev_tab: ChevronLeft, next_tab: ChevronRight, close_pane: SquareX,
}

const LAST = 'corral.panelPage'
export function lastPanelPage(): string | null {
  try {
    return localStorage.getItem(LAST)
  } catch {
    return null
  }
}

// Above the key bar, over the terminal. A press on its buttons takes no
// focus, so an on-screen keyboard that is up stays up (keys still reach
// them: Tab, Enter); a pick closes it, as do Escape and a press outside.
export function ShortcutPanel(p: { pages: PanelPage[]; open: PanelPage; herdrKeys?: Record<string, string>; keysUnknown?: boolean; onPick: (s: Shortcut) => void; onClose: () => void }) {
  const [page, setPage] = useState<PanelPage>(p.open)
  const ref = useRef<HTMLDivElement>(null)
  const { onClose } = p
  useEffect(() => {
    // (The key that opens it closes it again, by itself.)
    const outside = (e: PointerEvent) => {
      const el = e.target as Element | null
      if (!ref.current?.contains(el) && !el?.closest?.('[data-sc-opener]')) onClose()
    }
    // Escape closes it and goes nowhere else (on the window, before the
    // terminal's own listeners on the document).
    // Tab from outside (the terminal's field) moves into it.
    const esc = (e: KeyboardEvent) => {
      if (e.key === 'Tab' && !ref.current?.contains(document.activeElement)) {
        e.preventDefault()
        e.stopImmediatePropagation()
        ref.current?.querySelector<HTMLElement>('.sc-tab[aria-selected="true"]')?.focus()
        return
      }
      if (e.key !== 'Escape') return
      e.preventDefault()
      e.stopImmediatePropagation()
      onClose()
    }
    document.addEventListener('pointerdown', outside, true)
    window.addEventListener('keydown', esc, true)
    // With keys in use (a hardware keyboard), focus goes into it.
    if (keysInUse()) ref.current?.querySelector<HTMLElement>('.sc-tab[aria-selected="true"]')?.focus()
    return () => {
      document.removeEventListener('pointerdown', outside, true)
      window.removeEventListener('keydown', esc, true)
    }
  }, [onClose])
  // The page shown has its tab in sight (a phone's width does not hold them all).
  useEffect(() => {
    const tab = ref.current?.querySelector<HTMLElement>('.sc-tab[aria-selected="true"]')
    const bar = tab?.parentElement
    if (!tab || !bar) return
    const t = tab.getBoundingClientRect()
    const b = bar.getBoundingClientRect()
    if (t.right > b.right) bar.scrollLeft += t.right - b.right
    else if (t.left < b.left) bar.scrollLeft -= b.left - t.left
  }, [page])
  const choose = (pg: PanelPage) => {
    setPage(pg)
    try {
      localStorage.setItem(LAST, pg)
    } catch {
      /* ignore */
    }
  }
  const keep = (e: React.MouseEvent) => e.preventDefault() // (focus stays where it is)
  const groups = pageGroups(page, p.herdrKeys, p.keysUnknown)
  const foot = pageCommon(groups, page)
  // The foot row repeats the page's most used ones, for when the page has to
  // scroll: a page seen whole does without it (ux1101 T15). Measured with
  // the foot away (the room it would take counts as the page's).
  const pageRef = useRef<HTMLDivElement>(null)
  const footRef = useRef<HTMLElement>(null)
  // (Told once a page, with the foot away, and only ever turned on for it:
  // measured with the foot there, a short screen's page — held at its
  // min-height — would take it away and back again, for ever — review.)
  const [footPage, setFootPage] = useState<typeof page | null>(null)
  const needFoot = footPage === page
  const [faded, setFaded] = useState(false) // more below in the page
  useLayoutEffect(() => {
    const el = pageRef.current
    if (!el) return
    const look = () => {
      if (!needFoot && el.scrollHeight > el.clientHeight + 1) setFootPage(page)
      setFaded(el.scrollTop + el.clientHeight < el.scrollHeight - 1)
    }
    look()
    const ro = new ResizeObserver(look)
    ro.observe(el)
    el.addEventListener('scroll', look, { passive: true })
    return () => {
      ro.disconnect()
      el.removeEventListener('scroll', look)
    }
  }, [page, needFoot])
  // Tabs: Left and Right (Home, End) move between them and show that page,
  // the selected one the only stop for Tab (ux1101 T17).
  const tabKeys = (e: React.KeyboardEvent) => {
    if (e.altKey || e.ctrlKey || e.metaKey) return
    const i = p.pages.indexOf(page)
    const next = e.key === 'ArrowRight' ? (i + 1) % p.pages.length : e.key === 'ArrowLeft' ? (i - 1 + p.pages.length) % p.pages.length : e.key === 'Home' ? 0 : e.key === 'End' ? p.pages.length - 1 : -1
    if (next < 0) return
    e.preventDefault()
    e.stopPropagation()
    choose(p.pages[next])
    requestAnimationFrame(() => ref.current?.querySelector<HTMLElement>('.sc-tab[aria-selected="true"]')?.focus())
  }
  const item = (s: Shortcut, i: number) => {
    const Icon = 'herdr' in s ? icons[s.herdr] : undefined
    return (
      <button key={i} type="button" className="sc-item" onMouseDown={keep} onClick={() => p.onPick(s)}>
        <span className="sc-label">{s.label || (Icon && <Icon size={16} aria-hidden="true" />)}</span>
        <span className="sc-desc">{t(s.desc)}</span>
      </button>
    )
  }
  return (
    <div ref={ref} className="sc-panel" role="dialog" aria-label={t('sc.title')}>
      <div className="sc-head">
        <div className="sc-tabs" role="tablist" aria-label={t('sc.title')} onKeyDown={tabKeys}>
          {p.pages.map((pg) => (
            <button
              key={pg}
              id={'sc-tab-' + pg}
              type="button"
              role="tab"
              aria-selected={pg === page}
              aria-controls="sc-page"
              tabIndex={pg === page ? 0 : -1}
              className="sc-tab"
              onMouseDown={keep}
              onClick={() => choose(pg)}
            >
              {pageTitles[pg]}
            </button>
          ))}
        </div>
        <button type="button" className="icon-btn sc-close" aria-label={t('common.close')} onMouseDown={keep} onClick={p.onClose}>
          <X size={16} />
        </button>
      </div>
      <div className={'sc-page' + (faded ? ' faded' : '')} id="sc-page" role="tabpanel" aria-labelledby={'sc-tab-' + page} ref={pageRef}>
        {groups.map((g) => (
          <section key={g.title} className="sc-group" aria-label={t(g.title)}>
            <h3 className="sc-group-title">{t(g.title)}</h3>
            <div className="sc-grid">{g.items.map(item)}</div>
          </section>
        ))}
      </div>
      {foot.length > 0 && needFoot && (
        <section className="sc-foot" aria-label={t('sc.gCommon')} ref={footRef}>
          <h3 className="sc-group-title">{t('sc.gCommon')}</h3>
          <div className="sc-row">{foot.map(item)}</div>
        </section>
      )}
    </div>
  )
}
