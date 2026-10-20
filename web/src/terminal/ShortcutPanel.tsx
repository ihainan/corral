import { useEffect, useRef, useState } from 'react'
import { ArrowDown, ArrowLeft, ArrowRight, ArrowUp, ChevronLeft, ChevronRight, Columns2, List, Maximize2, Plus, Rows2, X } from 'lucide-react'
import { t } from '../i18n'
import { keysInUse } from '../components/ui'
import { pageTitles, shortcuts, type HerdrAct, type PanelPage, type Shortcut } from './shortcuts'

const icons: Partial<Record<HerdrAct, typeof Plus>> = {
  new_tab: Plus, split_right: Columns2, split_down: Rows2, zoom_toggle: Maximize2,
  focus_left: ArrowLeft, focus_right: ArrowRight, focus_up: ArrowUp, focus_down: ArrowDown,
  prev_tab: ChevronLeft, next_tab: ChevronRight, agents: List,
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
export function ShortcutPanel(p: { pages: PanelPage[]; open: PanelPage; onPick: (s: Shortcut) => void; onClose: () => void }) {
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
  const choose = (pg: PanelPage) => {
    setPage(pg)
    try {
      localStorage.setItem(LAST, pg)
    } catch {
      /* ignore */
    }
  }
  const keep = (e: React.MouseEvent) => e.preventDefault() // (focus stays where it is)
  return (
    <div ref={ref} className="sc-panel" role="dialog" aria-label={t('sc.title')}>
      <div className="sc-head">
        <div className="sc-tabs" role="tablist">
          {p.pages.map((pg) => (
            <button key={pg} type="button" role="tab" aria-selected={pg === page} className="sc-tab" onMouseDown={keep} onClick={() => choose(pg)}>
              {pageTitles[pg]}
            </button>
          ))}
        </div>
        <button type="button" className="icon-btn sc-close" aria-label={t('common.close')} onMouseDown={keep} onClick={p.onClose}>
          <X size={16} />
        </button>
      </div>
      <div className="sc-grid" role="tabpanel" aria-label={pageTitles[page]}>
        {shortcuts[page].map((s, i) => {
          const Icon = 'herdr' in s ? icons[s.herdr] : undefined
          return (
            <button key={i} type="button" className="sc-item" onMouseDown={keep} onClick={() => p.onPick(s)}>
              <span className="sc-label">{s.label || (Icon && <Icon size={16} aria-hidden="true" />)}</span>
              <span className="sc-desc">{t(s.desc)}</span>
            </button>
          )
        })}
      </div>
    </div>
  )
}
