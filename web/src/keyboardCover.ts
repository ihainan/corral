// iOS 26 on an iPhone does not take the whole keyboard off the visual
// viewport: the floating form bar (↑ ↓ ✓) and the row of suggestions or
// candidates over the keys are drawn over the page's foot. A real screenshot
// (11-24, iPhone, the Chinese 9-key keyboard) had the visual viewport end
// about 130pt below the top of that bar, the composer's buttons under it.
// No API tells their height: while an on-screen keyboard is up, this much is
// kept clear at the foot of what follows the visual viewport. (iPadOS draws
// its bar inside the keyboard; older iOS took it off.)
const iPhone = /iPhone/.test(navigator.userAgent)
// (WebKit 26: the first with anchor positioning. Chrome and others on an iPhone are WebKit too.)
const webkit26 = typeof CSS !== 'undefined' && typeof CSS.supports === 'function' && CSS.supports('anchor-name: --a')
export const KEYBOARD_COVER = iPhone && webkit26 ? 130 : 0

// Whether an on-screen keyboard is up: the visual viewport, unzoomed, at
// least 120px shorter than the tallest seen at this width. Not innerHeight
// against it: iOS 26 on an iPhone (11-24) may shrink the layout viewport
// with the keyboard too, leaving the two equal with the keyboard up. (Pinch
// zoom also shrinks the visual viewport: never taken for a keyboard.)
const tallest = new Map<number, number>()
export function keyboardIsUp(): boolean {
  const vv = window.visualViewport
  if (!vv || (vv.scale ?? 1) > 1.01) return false
  const w = Math.round(window.innerWidth)
  const h = Math.max(vv.height, window.innerHeight)
  const top = Math.max(tallest.get(w) ?? 0, h)
  tallest.set(w, top)
  return top - vv.height > 120 || window.innerHeight - vv.height > 120
}

// The height to lay a page out in: the visual viewport's, less what the
// keyboard's bars cover while it is up.
export const usableHeight = (vvHeight: number, keyboardUp: boolean) => Math.max(200, Math.round(vvHeight - (keyboardUp ? KEYBOARD_COVER : 0)))

// TEMPORARY (11-24): the numbers, shown on an iPhone while the keyboard is
// up, to set KEYBOARD_COVER from a screenshot. Remove once set.
export function showKeyboardNumbers(keyboardUp: boolean) {
  if (!iPhone) return
  let el = document.getElementById('kb-numbers')
  const a = document.activeElement
  const editing = !!a && (a.tagName === 'TEXTAREA' || a.tagName === 'INPUT' || (a as HTMLElement).isContentEditable)
  if (!keyboardUp && !editing) {
    el?.remove()
    return
  }
  const vv = window.visualViewport
  if (!el) {
    el = document.createElement('div')
    el.id = 'kb-numbers'
    el.style.cssText = 'position:fixed;left:0;right:0;top:env(safe-area-inset-top,0px);z-index:9999;pointer-events:none;font:11px/1.3 ui-monospace,monospace;background:rgba(255,230,120,.92);color:#000;padding:2px 6px;white-space:pre'
    document.body.appendChild(el)
  }
  const nav = navigator as Navigator & { standalone?: boolean }
  el.textContent = `ih ${window.innerHeight} vvh ${vv ? Math.round(vv.height) : '-'} vvtop ${vv ? Math.round(vv.offsetTop) : '-'} sh ${screen.height} top ${tallest.get(Math.round(window.innerWidth)) ?? '-'} kb ${keyboardUp ? 1 : 0} cover ${KEYBOARD_COVER} app ${nav.standalone ? 1 : 0} w26 ${webkit26 ? 1 : 0}`
}
