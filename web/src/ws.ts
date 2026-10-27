// One WebSocket per browser tab: terminal channels and events.
// Binary frame: 2-byte channel number (BE) followed by terminal bytes.

export interface ChannelHandler {
  onData(data: Uint8Array): void
  onOpened?(): void
  onWinsize?(cols: number, rows: number): void
  onClosed?(error?: string): void
}

interface Chan {
  agent: string
  session: string
  cols: number
  rows: number
  h: ChannelHandler
}

type Listener = (msg: any) => void

const enc = new TextEncoder()

export class Socket {
  private ws: WebSocket | null = null
  private chans = new Map<number, Chan>()
  private nextCh = 1
  private listeners = new Set<Listener>()
  private retry = 0
  private agents: string[] = []
  private focus: string[] = []
  private watches = new Map<string, number>()
  private stopped = false
  connected = false
  onState?: (connected: boolean) => void

  start() {
    this.stopped = false
    this.connect()
    document.addEventListener('visibilitychange', () => this.sendFocus())
  }

  stop() {
    this.stopped = true
    this.ws?.close()
  }

  private connect() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const ws = new WebSocket(`${proto}//${location.host}/api/ws`)
    ws.binaryType = 'arraybuffer'
    this.ws = ws
    ws.onopen = () => {
      this.retry = 0
      this.connected = true
      this.onState?.(true)
      // After a drop: resubscribe and reopen all channels.
      this.json({ t: 'subscribe', agents: this.agents })
      this.sendFocus()
      for (const k of this.watches.keys()) {
        const [agent, folder] = splitKey(k)
        this.json({ t: 'watch', agent, folder })
      }
      for (const [ch, c] of this.chans) this.json({ t: 'open', ch, agent: c.agent, session: c.session, cols: c.cols, rows: c.rows })
    }
    ws.onmessage = (ev) => {
      if (ev.data instanceof ArrayBuffer) {
        const b = new Uint8Array(ev.data)
        if (b.length < 2) return
        const ch = (b[0] << 8) | b[1]
        this.chans.get(ch)?.h.onData(b.subarray(2))
        return
      }
      let m: any
      try {
        m = JSON.parse(ev.data)
      } catch {
        return
      }
      if (m.t === 'opened') this.chans.get(m.ch)?.h.onOpened?.()
      else if (m.t === 'winsize') this.chans.get(m.ch)?.h.onWinsize?.(m.cols, m.rows)
      else if (m.t === 'closed') {
        const c = this.chans.get(m.ch)
        if (c) {
          this.chans.delete(m.ch)
          c.h.onClosed?.(m.error)
        }
      }
      for (const l of this.listeners) l(m)
    }
    ws.onclose = () => {
      this.connected = false
      this.onState?.(false)
      this.ws = null
      if (this.stopped) return
      const delay = Math.min(30000, 500 * 2 ** this.retry++) * (0.75 + Math.random() / 2)
      setTimeout(() => this.connect(), delay)
    }
  }

  private json(v: unknown) {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(v))
  }

  on(l: Listener): () => void {
    this.listeners.add(l)
    return () => this.listeners.delete(l)
  }

  subscribe(agents: string[]) {
    this.agents = agents
    this.json({ t: 'subscribe', agents })
  }

  setFocus(focus: string[]) {
    this.focus = focus
    this.sendFocus()
  }

  private sendFocus() {
    this.json({ t: 'focus', focus: this.focus, visible: document.visibilityState === 'visible' })
  }

  watch(agent: string, folder: string) {
    const k = `${agent}/${folder}`
    const n = this.watches.get(k) ?? 0
    this.watches.set(k, n + 1)
    if (n === 0) this.json({ t: 'watch', agent, folder })
  }

  unwatch(agent: string, folder: string) {
    const k = `${agent}/${folder}`
    const n = this.watches.get(k) ?? 0
    if (n <= 1) {
      this.watches.delete(k)
      this.json({ t: 'unwatch', agent, folder })
    } else this.watches.set(k, n - 1)
  }

  open(agent: string, session: string, cols: number, rows: number, h: ChannelHandler): number {
    const ch = this.nextCh++
    if (this.nextCh > 65000) this.nextCh = 1
    this.chans.set(ch, { agent, session, cols, rows, h })
    this.json({ t: 'open', ch, agent, session, cols, rows })
    return ch
  }

  close(ch: number) {
    if (this.chans.delete(ch)) this.json({ t: 'close', ch })
  }

  resize(ch: number, cols: number, rows: number) {
    const c = this.chans.get(ch)
    if (!c) return
    c.cols = cols
    c.rows = rows
    this.json({ t: 'resize', ch, cols, rows })
  }

  send(ch: number, data: string) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN || !this.chans.has(ch)) return
    const payload = enc.encode(data)
    const buf = new Uint8Array(2 + payload.length)
    buf[0] = ch >> 8
    buf[1] = ch & 0xff
    buf.set(payload, 2)
    this.ws.send(buf)
  }
}

function splitKey(k: string): [string, string] {
  const i = k.indexOf('/')
  return [k.slice(0, i), k.slice(i + 1)]
}

export const socket = new Socket()
