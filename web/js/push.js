// Web Push plumbing.
//
// Two invariants from the plans:
//   - on iOS, push exists only inside the installed (home-screen) app, and the
//     permission prompt must come from a user gesture — never on load;
//   - every app open pings /push/confirm, because a dead iOS subscription keeps
//     returning success to the server and is otherwise undetectable.

import { api } from './api.js';

export function pushSupported() {
  return 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
}

export function notificationPermission() {
  if (!('Notification' in window)) return 'unsupported';
  return Notification.permission; // 'default' | 'granted' | 'denied'
}

export function isIOS() {
  const ua = navigator.userAgent || '';
  const iPhoneish = /iPad|iPhone|iPod/.test(ua);
  // iPadOS 13+ reports as a Mac; the touch points give it away.
  const iPadDesktopUA = /Macintosh/.test(ua) && typeof navigator.maxTouchPoints === 'number' && navigator.maxTouchPoints > 1;
  return iPhoneish || iPadDesktopUA;
}

export function isStandalone() {
  if (typeof navigator.standalone === 'boolean' && navigator.standalone) return true;
  return typeof matchMedia === 'function' && matchMedia('(display-mode: standalone)').matches;
}

/** iOS Safari, opened as a tab: push cannot work until it is installed. */
export function needsIOSInstall() {
  return isIOS() && !isStandalone();
}

function base64UrlToUint8Array(base64) {
  const padded = String(base64).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(padded + '='.repeat((4 - (padded.length % 4)) % 4));
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

function bufferToBase64Url(buffer) {
  const bytes = new Uint8Array(buffer);
  let s = '';
  for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/** A short device name so the settings screen can list "iPhone · Safari". */
export function uaLabel() {
  const ua = navigator.userAgent || '';
  let device = 'Web';
  if (isIOS()) device = /iPad/.test(ua) ? 'iPad' : 'iPhone';
  else if (/Android/.test(ua)) device = 'Android';
  else if (/Macintosh/.test(ua)) device = 'Mac';
  else if (/Windows/.test(ua)) device = 'Windows';
  else if (/Linux/.test(ua)) device = 'Linux';

  let browser = 'Browser';
  if (/EdgA?\//.test(ua)) browser = 'Edge';
  else if (/OPR\//.test(ua)) browser = 'Opera';
  else if (/Firefox\//.test(ua)) browser = 'Firefox';
  else if (/Chrome\//.test(ua)) browser = 'Chrome';
  else if (/Safari\//.test(ua)) browser = 'Safari';

  const suffix = isStandalone() ? ' · PWA' : '';
  return `${device} · ${browser}${suffix}`;
}

export async function registration() {
  if (!('serviceWorker' in navigator)) return null;
  try {
    return await navigator.serviceWorker.ready;
  } catch (_) {
    return null;
  }
}

export async function currentSubscription() {
  const reg = await registration();
  if (!reg || !reg.pushManager) return null;
  try {
    return await reg.pushManager.getSubscription();
  } catch (_) {
    return null;
  }
}

function subscriptionBody(sub) {
  const json = typeof sub.toJSON === 'function' ? sub.toJSON() : null;
  const keys = (json && json.keys) || {};
  return {
    endpoint: sub.endpoint,
    p256dh: keys.p256dh || (sub.getKey ? bufferToBase64Url(sub.getKey('p256dh')) : ''),
    auth: keys.auth || (sub.getKey ? bufferToBase64Url(sub.getKey('auth')) : ''),
    ua_label: uaLabel(),
  };
}

/**
 * Ask for permission (caller must be inside a user gesture), subscribe, and
 * register the endpoint with the server.
 * Returns 'enabled' | 'blocked' | 'unsupported'.
 */
export async function enablePush(vapidPublicKey) {
  if (!pushSupported()) return 'unsupported';
  if (needsIOSInstall()) return 'unsupported';

  let permission = Notification.permission;
  if (permission === 'default') permission = await Notification.requestPermission();
  if (permission !== 'granted') return 'blocked';

  const reg = await registration();
  if (!reg || !reg.pushManager) return 'unsupported';

  let sub = await reg.pushManager.getSubscription();
  if (!sub) {
    sub = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: base64UrlToUint8Array(vapidPublicKey),
    });
  }
  await api.pushSubscribe(subscriptionBody(sub));
  return 'enabled';
}

/** Unsubscribe this device and forget it server-side (also used on logout). */
export async function disablePush() {
  const sub = await currentSubscription();
  if (!sub) return;
  const endpoint = sub.endpoint;
  try {
    await sub.unsubscribe();
  } catch (_) { /* already gone */ }
  try {
    await api.pushUnsubscribe(endpoint);
  } catch (_) { /* offline or already pruned */ }
}

/**
 * Liveness check, called on every app open. Confirms the endpoint the server
 * knows about, and silently repairs a subscription that the OS dropped while
 * permission is still granted (the iOS failure mode).
 */
export async function confirmPush(vapidPublicKey) {
  if (!pushSupported() || Notification.permission !== 'granted') return null;
  const reg = await registration();
  if (!reg || !reg.pushManager) return null;

  let sub = await reg.pushManager.getSubscription();
  if (!sub && vapidPublicKey) {
    try {
      sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: base64UrlToUint8Array(vapidPublicKey),
      });
      await api.pushSubscribe(subscriptionBody(sub));
      return sub.endpoint;
    } catch (_) {
      return null;
    }
  }
  if (!sub) return null;
  try {
    await api.pushConfirm(sub.endpoint);
  } catch (_) { /* offline: the next open retries */ }
  return sub.endpoint;
}
