import { PushEventClient, type SocketLike } from './pushEventsLogic'

// Per-tab singleton WebSocket client for /api/events/ws hint push.

function eventsWsURL(): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${proto}//${location.host}/api/events/ws`
}

const client = new PushEventClient({
  createSocket: () => new WebSocket(eventsWsURL()) as unknown as SocketLike,
  isHidden: () => typeof document !== 'undefined' && document.hidden,
})

if (typeof window !== 'undefined') {
  window.addEventListener('online', () => client.reconnectNow())
}

export function setPushEventsActive(active: boolean) {
  client.setActive(active)
}

export function watchPushHosts(source: symbol, hosts: number[]): () => void {
  return client.watchHosts(source, hosts)
}

export function isPushConnected(): boolean {
  return client.isConnected()
}

export function isPushHostCovered(hostId: number | undefined): boolean {
  return client.isHostCovered(hostId)
}

export function arePushHostsCovered(hostIds: number[]): boolean {
  return client.areAllHostsCovered(hostIds)
}

export function onPushConvo(fn: (session: string, pane: string) => void): () => void {
  return client.onConvo(fn)
}

export function onPushAgents(fn: (host: number) => void): () => void {
  return client.onAgents(fn)
}

export function onPushStateChange(fn: () => void): () => void {
  return client.onStateChange(fn)
}
