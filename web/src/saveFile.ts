// A file made in the page (not one the gateway serves), saved on the device.
//
// Where the browser lets the page ask (Chrome's and Edge's on a computer), the
// system's save dialog is opened directly: what becomes of the file is then
// known, and nothing between the page and the disk can drop it (a download link
// did nothing at all in one person's Chrome, with no cause to be seen from here:
// an extension taking downloads over and a site's download setting look alike).
// A link with "download" gives no sign of itself in most browsers, and in an
// app installed from Safari (the iPhone's or iPad's Home Screen, a Mac's "Add
// to Dock") it can do nothing: there the system's share sheet saves a file.
// So the caller is told which happened, and says so.

const standalone = () => window.matchMedia('(display-mode: standalone)').matches || (navigator as { standalone?: boolean }).standalone === true
// (Safari's, not another browser's app: Chrome's and Edge's installed apps download as a tab does.)
const webKit = () => /AppleWebKit/.test(navigator.userAgent) && !/Chrome|Chromium|CriOS|Edg|OPR/.test(navigator.userAgent)

type SavePicker = (o: { suggestedName: string }) => Promise<{ name: string; createWritable: () => Promise<{ write: (b: Blob) => Promise<void>; close: () => Promise<void> }> }>

// 'saved': written where the person chose (as: the name they gave it).
// 'started': handed to the browser, which does not say whether it was kept.
// 'shared': the share sheet was gone through. 'cancelled': the person closed it.
// 'failed': not saved, and the words should be got another way.
export type Saved = { how: 'saved'; as: string } | { how: 'started' | 'shared' | 'cancelled' | 'failed' }

export async function saveFile(name: string, blob: Blob): Promise<Saved> {
  const picker = (window as { showSaveFilePicker?: SavePicker }).showSaveFilePicker
  if (picker) {
    let chosen: Awaited<ReturnType<SavePicker>> | undefined
    try {
      chosen = await picker.call(window, { suggestedName: name })
      const out = await chosen.createWritable()
      await out.write(blob)
      await out.close()
      return { how: 'saved', as: chosen.name }
    } catch (e) {
      if (e instanceof DOMException && e.name === 'AbortError') return { how: 'cancelled' }
      // (A place was chosen and could not be written: said so, nothing else tried behind the person's back.)
      if (chosen) return { how: 'failed' }
      // (Not allowed now, as long after the click, with a conversation read in first: the link below.)
    }
  }
  if (standalone() && webKit()) {
    const file = new File([blob], name, { type: blob.type })
    if (!navigator.canShare?.({ files: [file] })) return { how: 'failed' }
    try {
      await navigator.share({ files: [file] })
      return { how: 'shared' }
    } catch (e) {
      return { how: e instanceof DOMException && e.name === 'AbortError' ? 'cancelled' : 'failed' }
    }
  }
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.appendChild(a)
  a.click()
  a.remove()
  // (Not soon: Safari reads it after the click has returned, and a large file takes a while.)
  setTimeout(() => URL.revokeObjectURL(url), 60000)
  return { how: 'started' }
}
