// Native-only settings transport. Ordinary browsers and Room iframes do not
// receive desktop privileges, even when their URL contains desktop=1.
(() => {
  'use strict';
  if (window !== window.top || new URLSearchParams(window.location.search).get('desktop') !== '1') return;
  const transport = window.chrome?.webview || window.webkit?.messageHandlers?.external;
  if (typeof transport?.postMessage !== 'function') return;
  // i18n.js loads first on the Management page; the English text remains only
  // for a page without it.
  const text = (key, fallback) => window.PairRoomI18n?.t(`desktop.${key}`) || fallback;
  const pending = new Map();
  const prefix = window.crypto.randomUUID();
  let sequence = 0;
  function request(action, enabled) {
    if (pending.size) return Promise.reject(new Error(text('requestPending', 'A desktop setting request is already pending')));
    return new Promise((resolve, reject) => {
      const id = `${prefix}:${++sequence}`;
      const timer = setTimeout(() => {
        pending.delete(id);
        reject(new Error(text('noResponse', 'Desktop startup settings did not respond. Reopen Settings to read the system state.')));
      }, 10000);
      pending.set(id, {resolve, reject, timer});
      try {
        transport.postMessage(JSON.stringify({kind: 'pairroom.desktop.startup', id, action, ...(action === 'set' ? {enabled} : {})}));
      } catch (error) {
        clearTimeout(timer); pending.delete(id); reject(error);
      }
    });
  }
  window.PairRoomDesktop = Object.freeze({
    readStartup: () => request('get'),
    setStartup(enabled) {
      if (typeof enabled !== 'boolean') return Promise.reject(new TypeError(text('invalidSetting', 'Startup setting must be a boolean')));
      return request('set', enabled);
    },
    receive(response) {
      const entry = pending.get(response?.id);
      if (!entry) return;
      clearTimeout(entry.timer); pending.delete(response.id);
      if (response.error) {
        const error = new Error(response.error);
        if (typeof response.enabled === 'boolean') error.enabled = response.enabled;
        entry.reject(error);
      } else if (typeof response.enabled !== 'boolean') {
        entry.reject(new Error(text('statusUnavailable', 'Desktop startup status is unavailable')));
      } else {
        entry.resolve(response.enabled);
      }
    },
  });
})();
