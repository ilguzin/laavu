import { useEffect, useState } from 'preact/hooks'
import { creationOptions, credentialJSON, enc, post, requestOptions } from '../api'
import { Icon } from '../icons'

function Brand() {
  return (
    <>
      <span class="logo">
        <Icon name="logo" size={84} stroke={1.4} />
      </span>
      <h1>Laavu</h1>
    </>
  )
}

function deviceLabel() {
  const ua = navigator.userAgent
  const os = /iPhone|iPad/.test(ua) ? 'iOS' : /Android/.test(ua) ? 'Android' : /Mac/.test(ua) ? 'macOS' : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : 'устройство'
  const br = /Firefox/.test(ua) ? 'Firefox' : /Edg\//.test(ua) ? 'Edge' : /Chrome/.test(ua) ? 'Chrome' : /Safari/.test(ua) ? 'Safari' : ''
  return `${os} ${br}`.trim()
}

async function finish(path: string, cred: Credential | null) {
  if (!cred) throw new Error('passkey не получен')
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
      setErr(webauthnError(e, 'Вход отменён'))
    } finally {
      setBusy(false)
    }
  }
  return (
    <div class="auth">
      <Brand />
      <p class="muted">Агенты работают на вашем сервере. Заходите, когда удобно.</p>
      <button class="primary big" disabled={busy} onClick={login}>
        <Icon name="user" size={19} stroke={2} />
        Войти с passkey
      </button>
      {err && <p class="error">{err}</p>}
      <p class="muted small">Нет passkey на этом устройстве — попросите ссылку «добавить устройство» на устройстве, где вы уже вошли.</p>
      <p class="muted small mono">{location.host}</p>
    </div>
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
      // InvalidStateError при создании: на устройстве уже есть passkey этого
      // пользователя (например, приехал через связку ключей iCloud). Новый не нужен.
      if (e?.name === 'InvalidStateError') setExists(true)
      else setErr(webauthnError(e, 'Регистрация отменена'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div class="auth">
      <Brand />
      {!info && !err && <p class="muted">Проверяю ссылку…</p>}
      {exists && (
        <>
          <p>На этом устройстве уже есть ваш passkey — например, он пришёл через связку ключей iCloud или менеджер паролей. Новый не нужен: просто войдите.</p>
          <button
            class="primary big"
            onClick={() => {
              history.replaceState(null, '', '/')
              onDone()
            }}
          >
            Перейти ко входу
          </button>
        </>
      )}
      {info && !exists && (
        <form onSubmit={register}>
          {info.new_user ? (
            <>
              <p>Регистрация{info.role === 'admin' ? ' администратора' : ''}. Как вас зовут?</p>
              <input required maxLength={64} value={info.name || name} disabled={!!info.name} onInput={(e) => setName((e.target as HTMLInputElement).value)} placeholder="имя" autoFocus />
            </>
          ) : (
            <p>Новое устройство для {info.name}.</p>
          )}
          <button class="primary big" type="submit" disabled={busy}>
            Создать passkey
          </button>
        </form>
      )}
      {err && <p class="error">{err}</p>}
    </div>
  )
}

// webauthnError — понятный текст ошибки WebAuthn. Под NotAllowedError браузер
// прячет и отмену пользователем, и недоверенный сертификат, и отсутствие
// подходящего аутентификатора, поэтому показываем и его собственное сообщение.
function webauthnError(e: any, cancelled: string): string {
  if (e?.name !== 'NotAllowedError') return e?.message ?? String(e)
  const hints = [
    'окно passkey закрыто или истекло время',
    'сертификат сайта не доверен браузером (замок в адресной строке)',
    'нет аутентификатора с PIN или биометрией: можно выбрать телефон по QR-коду',
  ]
  return `${cancelled}. Возможные причины: ${hints.join('; ')}. Браузер сообщил: ${e.message}`
}
