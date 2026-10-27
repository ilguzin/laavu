import type { ComponentChildren } from 'preact'
import { useEffect, useState } from 'preact/hooks'
import { creationOptions, credentialJSON, enc, post, requestOptions } from '../api'
import { Icon } from '../icons'

// AuthPage: sign-in and registration. The left panel says which server this is (hidden on
// narrow screens, where the host sits above the form); nothing about the server is shown
// before sign-in beyond its address.
function AuthPage({ title, children }: { title?: string; children: ComponentChildren }) {
  const brand = (
    <span class="auth-brand">
      <Icon name="logo" size={24} stroke={2.2} />
      <span>laavu</span>
    </span>
  )
  return (
    <div class="auth">
      <section class="auth-id">
        {brand}
        <div class="auth-where">
          <p class="auth-kicker">You are signing in to</p>
          <p class="auth-host mono">{location.host}</p>
          <p class="auth-tag">Agents run on your server. Drop in whenever you like.</p>
        </div>
        <nav class="auth-links">
          <a href="https://getlaavu.com" target="_blank" rel="noopener">
            getlaavu.com
          </a>
          <a href="https://github.com/ilguzin/laavu" target="_blank" rel="noopener">
            GitHub
          </a>
        </nav>
        <span class="auth-mark" aria-hidden="true">
          <Icon name="logo" size={120} stroke={1.2} />
        </span>
      </section>
      <section class="auth-side">
        <header>{brand}</header>
        <div class="auth-form">
          <span class="auth-chip mono">
            <i />
            {location.host}
          </span>
          {title && <h1>{title}</h1>}
          {children}
        </div>
      </section>
    </div>
  )
}

function deviceLabel() {
  const ua = navigator.userAgent
  const os = /iPhone|iPad/.test(ua) ? 'iOS' : /Android/.test(ua) ? 'Android' : /Mac/.test(ua) ? 'macOS' : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : 'device'
  const br = /Firefox/.test(ua) ? 'Firefox' : /Edg\//.test(ua) ? 'Edge' : /Chrome/.test(ua) ? 'Chrome' : /Safari/.test(ua) ? 'Safari' : ''
  return `${os} ${br}`.trim()
}

async function finish(path: string, cred: Credential | null) {
  if (!cred) throw new Error('no passkey received')
  const r = await fetch(path, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(credentialJSON(cred as PublicKeyCredential)),
  })
  const d = await r.json().catch(() => ({}))
  if (!r.ok) throw new Error(d.error || r.statusText)
  return d
}

export function Login({ onDone }: { onDone: () => void }) {
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  async function login() {
    setErr('')
    setBusy(true)
    try {
      const b = await post('/api/auth/login/begin')
      const cred = await navigator.credentials.get(requestOptions(b.options))
      await finish(`/api/auth/login/finish?cid=${enc(b.cid)}`, cred)
      onDone()
    } catch (e: any) {
      setErr(webauthnError(e, 'Sign-in cancelled'))
    } finally {
      setBusy(false)
    }
  }
  return (
    <AuthPage title="Sign in">
      <p class="muted">Use the passkey you made when you joined this server.</p>
      <button class="primary big" disabled={busy} onClick={login}>
        <Icon name="user" size={19} stroke={2} />
        Sign in with passkey
      </button>
      {err && <p class="error">{err}</p>}
      <p class="muted small">No passkey on this device? Ask for an "add device" link on a device where you are already signed in.</p>
    </AuthPage>
  )
}

export function Register({ onDone }: { onDone: () => void }) {
  const token = location.hash.slice(1)
  const [info, setInfo] = useState<{ new_user: boolean; name?: string; role: string } | null>(null)
  const [name, setName] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [exists, setExists] = useState(false)

  useEffect(() => {
    post('/api/auth/register/check', { token })
      .then(setInfo)
      .catch((e) => setErr(e.message))
  }, [token])

  async function register(e: Event) {
    e.preventDefault()
    setErr('')
    setBusy(true)
    try {
      const b = await post('/api/auth/register/begin', { token, name })
      const cred = await navigator.credentials.create(creationOptions(b.options))
      await finish(`/api/auth/register/finish?cid=${enc(b.cid)}&label=${enc(deviceLabel())}`, cred)
      history.replaceState(null, '', '/')
      onDone()
    } catch (e: any) {
      // InvalidStateError on create: the device already has this user's passkey
      // (for example, synced via iCloud Keychain). No new one is needed.
      if (e?.name === 'InvalidStateError') setExists(true)
      else setErr(webauthnError(e, 'Registration cancelled'))
    } finally {
      setBusy(false)
    }
  }

  const title = exists ? 'Already set up' : !info ? undefined : info.new_user ? 'Join this server' : 'Add this device'
  return (
    <AuthPage title={title}>
      {!info && !err && <p class="muted">Checking the link…</p>}
      {exists && (
        <>
          <p>This device already has your passkey, for example synced via iCloud Keychain or a password manager. No new one is needed: just sign in.</p>
          <button
            class="primary big"
            onClick={() => {
              history.replaceState(null, '', '/')
              onDone()
            }}
          >
            Go to sign in
          </button>
        </>
      )}
      {info && !exists && (
        <form onSubmit={register}>
          {info.new_user ? (
            <>
              <p class="muted">
                {info.role === 'admin' ? 'You are invited as the admin.' : 'You are invited to this server.'} The passkey stays on this device, there is no password.
              </p>
              <label>
                Your name
                <input required maxLength={64} value={info.name || name} disabled={!!info.name} onInput={(e) => setName((e.target as HTMLInputElement).value)} autoFocus autoComplete="name" />
              </label>
            </>
          ) : (
            <p class="muted">A new passkey for {info.name} on this device.</p>
          )}
          <button class="primary big" type="submit" disabled={busy}>
            Create passkey
          </button>
        </form>
      )}
      {err && <p class="error">{err}</p>}
    </AuthPage>
  )
}

// webauthnError: a readable WebAuthn error. NotAllowedError hides user
// cancellation, an untrusted certificate and a missing suitable authenticator
// alike, so the browser's own message is shown too.
function webauthnError(e: any, cancelled: string): string {
  if (e?.name !== 'NotAllowedError') return e?.message ?? String(e)
  const hints = [
    'the passkey dialog was closed or timed out',
    'the browser does not trust the site certificate (see the lock in the address bar)',
    'no authenticator with a PIN or biometrics: you can pick a phone via QR code',
  ]
  return `${cancelled}. Possible causes: ${hints.join('; ')}. Browser said: ${e.message}`
}
