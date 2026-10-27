// Daemon HTTP API calls.

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message)
  }
}

export async function api<T = any>(method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: 'same-origin', headers: {} }
  if (body !== undefined) {
    init.body = JSON.stringify(body)
    ;(init.headers as Record<string, string>)['Content-Type'] = 'application/json'
  }
  const r = await fetch(path, init)
  let data: any = null
  const text = await r.text()
  try {
    data = text ? JSON.parse(text) : null
  } catch {
    data = { error: text }
  }
  if (!r.ok) throw new ApiError(r.status, (data && data.error) || r.statusText)
  return data as T
}

export const get = <T = any>(p: string) => api<T>('GET', p)
export const post = <T = any>(p: string, b?: unknown) => api<T>('POST', p, b ?? {})

export const enc = encodeURIComponent

export interface Me {
  name: string
  role: string
  no_auth: boolean
  view_url: string
  push: boolean
  // Last agent isolation check (laavu check-isolation), admin only.
  isolation?: { time: number; pairs: number; failed: number } | null
}

export interface Agent {
  name: string
  ports: string
}

export interface Workdir {
  name: string
  mtime: number
  git: boolean
  branch?: string
  changes?: number
}

// The agent's public ssh key; empty when it has none yet.
export interface SSHKey {
  public?: string
  fingerprint?: string
}

// The agent's last action: tool and path from PreToolUse.
export interface Action {
  tool: string
  target?: string
  at: number
}

export interface SessionInfo {
  name: string
  folder: string
  path: string
  kind: 'claude' | 'shell' | 'other'
  author?: string
  claude?: string
  status?: string
  sleeping?: boolean
  dead?: boolean
  attached: number
  activity?: number
  created?: number
  command?: string
  action?: Action
}

export interface Entry {
  name: string
  type: 'dir' | 'file' | 'link' | 'other'
  size: number
  mtime: number
  hidden?: boolean
  kind?: string
}

export interface FileMeta {
  path: string
  size: number
  mtime: number
  kind: string
  dir?: boolean
}

export interface ContextUsage {
  used: number
  window: number
  percent: number
  model?: string
  source: string
}

export interface FileChange {
  status?: string
  path: string
  mtime?: number
}

export interface Changes {
  git: boolean
  commits?: { hash: string; author: string; time: number; subject: string; files: FileChange[] }[]
  uncommitted?: FileChange[]
  files?: FileChange[]
  truncated?: boolean
}

export const agentPath = (a: string) => `/api/agents/${enc(a)}`
export const sessPath = (a: string, s: string) => `${agentPath(a)}/sessions/${enc(s)}`
export const filesPath = (a: string, f: string) => `${agentPath(a)}/files/${enc(f)}`

// --- WebAuthn: options from the server arrive as base64url ---

export function b64uToBuf(s: string): ArrayBuffer {
  const pad = '='.repeat((4 - (s.length % 4)) % 4)
  const b = atob((s + pad).replace(/-/g, '+').replace(/_/g, '/'))
  const out = new Uint8Array(b.length)
  for (let i = 0; i < b.length; i++) out[i] = b.charCodeAt(i)
  return out.buffer
}

export function bufToB64u(buf: ArrayBuffer | null): string {
  if (!buf) return ''
  const b = new Uint8Array(buf)
  let s = ''
  for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i])
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

export function creationOptions(opts: any): CredentialCreationOptions {
  const pk = { ...opts.publicKey }
  pk.challenge = b64uToBuf(pk.challenge)
  pk.user = { ...pk.user, id: b64uToBuf(pk.user.id) }
  if (pk.excludeCredentials) pk.excludeCredentials = pk.excludeCredentials.map((c: any) => ({ ...c, id: b64uToBuf(c.id) }))
  return { publicKey: pk }
}

export function requestOptions(opts: any): CredentialRequestOptions {
  const pk = { ...opts.publicKey }
  pk.challenge = b64uToBuf(pk.challenge)
  if (pk.allowCredentials) pk.allowCredentials = pk.allowCredentials.map((c: any) => ({ ...c, id: b64uToBuf(c.id) }))
  const out: CredentialRequestOptions = { publicKey: pk }
  if (opts.mediation) out.mediation = opts.mediation
  return out
}

export function credentialJSON(c: PublicKeyCredential): any {
  const r: any = c.response
  const res: any = { clientDataJSON: bufToB64u(r.clientDataJSON) }
  if (r.attestationObject) {
    res.attestationObject = bufToB64u(r.attestationObject)
    if (r.getTransports) res.transports = r.getTransports()
  }
  if (r.authenticatorData) {
    res.authenticatorData = bufToB64u(r.authenticatorData)
    res.signature = bufToB64u(r.signature)
    res.userHandle = bufToB64u(r.userHandle)
  }
  return {
    id: c.id,
    rawId: bufToB64u(c.rawId),
    type: c.type,
    authenticatorAttachment: (c as any).authenticatorAttachment ?? undefined,
    clientExtensionResults: c.getClientExtensionResults(),
    response: res,
  }
}

// --- Upload with progress ---

export function upload(agent: string, folder: string, file: File, dir: string, onProgress?: (p: number) => void): Promise<{ path: string; size: number }> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    const q = `name=${enc(file.name || 'file')}` + (dir ? `&dir=${enc(dir)}` : '')
    xhr.open('POST', `${filesPath(agent, folder)}/upload?${q}`)
    xhr.upload.onprogress = (e) => e.lengthComputable && onProgress?.(e.loaded / e.total)
    xhr.onload = () => {
      let data: any = {}
      try {
        data = JSON.parse(xhr.responseText)
      } catch {}
      if (xhr.status === 200) resolve(data)
      else reject(new ApiError(xhr.status, data.error || xhr.statusText))
    }
    xhr.onerror = () => reject(new ApiError(0, 'network unavailable'))
    xhr.send(file)
  })
}

// --- localStorage guarded against exceptions ---

export function lsGet(k: string): string | null {
  try {
    return localStorage.getItem(k)
  } catch {
    return null
  }
}

export function lsSet(k: string, v: string) {
  try {
    localStorage.setItem(k, v)
  } catch {}
}
