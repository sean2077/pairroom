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
  // Each setting has one request lane, so a slow update check never blocks
  // reading or changing launch at login.
  function request(lane, payload, timeoutMs, settle) {
    for (const entry of pending.values()) {
      if (entry.lane === lane) return Promise.reject(new Error(text('requestPending', 'A desktop setting request is already pending')));
    }
    return new Promise((resolve, reject) => {
      const id = `${prefix}:${++sequence}`;
      const timer = setTimeout(() => {
        pending.delete(id);
        reject(new Error(text('noResponse', 'Desktop settings did not respond. Reopen Settings to read the system state.')));
      }, timeoutMs);
      pending.set(id, {lane, settle, resolve, reject, timer});
      try {
        transport.postMessage(JSON.stringify({kind: lane, id, ...payload}));
      } catch (error) {
        clearTimeout(timer); pending.delete(id); reject(error);
      }
    });
  }
  function startup(action, enabled) {
    return request('pairroom.desktop.startup', {action, ...(action === 'set' ? {enabled} : {})}, 10000, (response) => {
      if (response.error) {
        const error = new Error(response.error);
        if (typeof response.enabled === 'boolean') error.enabled = response.enabled;
        throw error;
      }
      if (typeof response.enabled !== 'boolean') throw new Error(text('statusUnavailable', 'Desktop startup status is unavailable'));
      return response.enabled;
    });
  }
  function updateStatus(response) {
    const string = (value) => typeof value === 'string' ? value : '';
    return {enabled: response.enabled, current: string(response.current), latest: string(response.latest),
      url: string(response.url), checkedAt: string(response.checked_at)};
  }
  function updates(action, enabled) {
    // The native request timeout is shorter, so a manual check reports its own failure.
    return request('pairroom.desktop.updates', {action, ...(action === 'set' ? {enabled} : {})}, 20000, (response) => {
      if (typeof response.enabled !== 'boolean') throw new Error(response.error || 'Desktop update status is unavailable');
      const status = updateStatus(response);
      if (response.error) throw Object.assign(new Error(response.error), {status});
      return status;
    });
  }
  window.PairRoomDesktop = Object.freeze({
    readStartup: () => startup('get'),
    setStartup(enabled) {
      if (typeof enabled !== 'boolean') return Promise.reject(new TypeError(text('invalidSetting', 'Startup setting must be a boolean')));
      return startup('set', enabled);
    },
    readUpdates: () => updates('get'),
    setUpdates(enabled) {
      if (typeof enabled !== 'boolean') return Promise.reject(new TypeError('Update setting must be a boolean'));
      return updates('set', enabled);
    },
    checkUpdates: () => updates('check'),
    // Uses the same native link path as Ctrl+click; the host accepts only http(s).
    openExternal(url) {
      transport.postMessage(JSON.stringify({kind: 'pairroom.desktop.browser', url: String(url)}));
    },
    receive(response) {
      const entry = pending.get(response?.id);
      if (!entry) return;
      clearTimeout(entry.timer); pending.delete(response.id);
      try {
        entry.resolve(entry.settle(response));
      } catch (error) {
        entry.reject(error);
      }
    },
  });
})();
