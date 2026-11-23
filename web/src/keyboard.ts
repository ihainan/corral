// Whether an on-screen keyboard is up: the visual viewport, unzoomed, at
// least 120px shorter than the tallest seen at this width, or than the
// layout viewport. Not the latter alone: iOS 26 in a Home Screen app
// shrinks the layout viewport with the keyboard too (seen 11-24 on an
// iPhone: innerHeight and visualViewport.height both 407 with it up, 873
// with it down), so the two stay equal. (Pinch zoom also shrinks the
// visual viewport: never taken for a keyboard.)
const tallest = new Map<number, number>()
export function keyboardIsUp(): boolean {
  const vv = window.visualViewport
  if (!vv || (vv.scale ?? 1) > 1.01) return false
  const w = Math.round(window.innerWidth)
  const top = Math.max(tallest.get(w) ?? 0, vv.height, window.innerHeight)
  tallest.set(w, top)
  return top - vv.height > 120 || window.innerHeight - vv.height > 120
}
