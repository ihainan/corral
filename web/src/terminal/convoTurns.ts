import type { Item } from './convoItems'

// A turn of the person's, as the gateway's index of the whole log gives it (GET /conversation/turns).
export type Turn = { at: number; ts?: string; preview: string }

// The item of a turn among those read in: the person's message of that
// time, beginning as it does. Of several alike (the same time, the same
// beginning): what is read in is the log's end, so those of them read in
// are the last of them in the log — in the same order; one of the first,
// not read in yet, is not one of those (review517/518). (Turns by place in
// the log, not by which object: the list is asked for anew meanwhile.)
export function itemOf(list: Item[], turns: Turn[], turn: Turn): string | null {
  if (!turn.ts) return null
  const alike = turns.filter((x) => x.ts === turn.ts && x.preview === turn.preview).sort((a, b) => a.at - b.at)
  const nth = alike.findIndex((x) => x.at === turn.at)
  if (nth < 0) return null
  // (Told alike as the index tells them: its preview of the same words —
  // not a beginning shared with a longer one, review520.)
  const keys = list.flatMap((it) => {
    if (it.kind !== 'user' || it.e.ts !== turn.ts) return []
    return previewOf(it.e.parts.filter((p) => p.kind === 'text').map((p) => p.text ?? '').join(' ')) === turn.preview ? ['e' + it.e.seq] : []
  })
  if (keys.length > alike.length) return null // (more read in than the log has: not told apart)
  const first = alike.length - keys.length // (of them, the first read in)
  return nth >= first ? keys[nth - first] : null
}

// previewOf: a message's preview as the gateway makes it (convo_turns.go
// preview): its white space as one, its first 120 characters.
export function previewOf(text: string): string {
  const flat = text.split(/\s+/).filter(Boolean).join(' ')
  const chars = [...flat]
  return chars.length <= 120 ? flat : chars.slice(0, 120).join('') + '…'
}
