// Web Push subscription on this device.
import { b64uToBuf, get, post } from './api'

export async function subscribePush(): Promise<string> {
  if (!('serviceWorker' in navigator) || !('PushManager' in window)) throw new Error('This browser does not support push. On iPhone, use "Add to Home Screen" first.')
  const perm = await Notification.requestPermission()
  if (perm !== 'granted') throw new Error('Notifications are blocked in the browser settings')
  const reg = await navigator.serviceWorker.ready
  const { key } = await get<{ key: string }>('/api/push/key')
  let sub = await reg.pushManager.getSubscription()
  if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64uToBuf(key) })
  await post('/api/push/subscribe', sub.toJSON())
  return 'Notifications enabled'
}
