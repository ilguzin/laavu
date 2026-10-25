// Подписка на Web Push на этом устройстве.
import { b64uToBuf, get, post } from './api'

export async function subscribePush(): Promise<string> {
  if (!('serviceWorker' in navigator) || !('PushManager' in window)) throw new Error('Браузер не умеет пуш. На iPhone — сначала «На экран Домой».')
  const perm = await Notification.requestPermission()
  if (perm !== 'granted') throw new Error('Уведомления запрещены в настройках браузера')
  const reg = await navigator.serviceWorker.ready
  const { key } = await get<{ key: string }>('/api/push/key')
  let sub = await reg.pushManager.getSubscription()
  if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64uToBuf(key) })
  await post('/api/push/subscribe', sub.toJSON())
  return 'Уведомления включены'
}
