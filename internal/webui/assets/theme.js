(() => {
  'use strict';
  const STORAGE_KEY = 'pairroom.theme';
  const MODES = ['system', 'light', 'dark'];
  const media = window.matchMedia('(prefers-color-scheme: dark)');
  let mode = readMode();

  function readMode() {
    try {
      const value = localStorage.getItem(STORAGE_KEY);
      if (MODES.includes(value)) return value;
    } catch (_error) { /* best effort */ }
    return 'system';
  }

  function resolved() { return mode === 'system' ? (media.matches ? 'dark' : 'light') : mode; }
  function updateControls() {
    document.querySelectorAll('[data-theme-value]').forEach((button) => {
      const selected = button.dataset.themeValue === mode;
      button.setAttribute('aria-pressed', String(selected));
      button.classList.toggle('active', selected);
    });
    document.querySelectorAll('#theme-button,[data-theme-cycle]').forEach((button) => {
      button.dataset.themeMode = mode;
      button.setAttribute('aria-label', window.PairRoomI18n ? window.PairRoomI18n.t(`theme.${mode}`) : mode);
      button.title = button.getAttribute('aria-label');
      button.textContent = mode === 'system' ? '◐' : mode === 'light' ? '☀' : '☾';
    });
  }
  function broadcast() {
    document.querySelectorAll('iframe').forEach((frame) => {
      try { frame.contentWindow.postMessage({ type: 'pairroom-theme', mode }, window.location.origin); } catch (_error) { /* detached frame */ }
    });
  }
  function apply() {
    document.documentElement.dataset.theme = resolved();
    document.documentElement.dataset.themeMode = mode;
    updateControls();
    broadcast();
    document.dispatchEvent(new CustomEvent('pairroom:theme', { detail: { mode, resolved: resolved() } }));
  }
  function setTheme(value, persist = true) {
    mode = MODES.includes(value) ? value : 'system';
    if (persist) {
      try { localStorage.setItem(STORAGE_KEY, mode); } catch (_error) { /* best effort */ }
    }
    apply();
  }
  function cycle() { setTheme(MODES[(MODES.indexOf(mode) + 1) % MODES.length]); }

  window.PairRoomTheme = { get mode() { return mode; }, get resolved() { return resolved(); }, setTheme, cycle, apply, STORAGE_KEY };
  document.addEventListener('click', (event) => {
    const explicit = event.target.closest('[data-theme-value]');
    if (explicit) { setTheme(explicit.dataset.themeValue); return; }
    if (event.target.closest('#theme-button,[data-theme-cycle]')) cycle();
  });
  window.addEventListener('storage', (event) => {
    if (event.key === STORAGE_KEY) setTheme(event.newValue || 'system', false);
  });
  window.addEventListener('message', (event) => {
    if (event.origin !== window.location.origin || event.data?.type !== 'pairroom-theme') return;
    setTheme(event.data.mode, false);
  });
  media.addEventListener?.('change', () => { if (mode === 'system') apply(); });
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', apply, { once: true });
  else apply();
})();
