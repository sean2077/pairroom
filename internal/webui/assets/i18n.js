(() => {
  'use strict';

  const STORAGE_KEY = 'pairroom.lang';
  function normalize(value) {
    const language = String(value || '').toLowerCase();
    if (language === 'zh' || language.startsWith('zh-')) return 'zh-CN';
    return 'en';
  }

  function detect() {
    try {
      const saved = localStorage.getItem(STORAGE_KEY);
      if (saved) return normalize(saved);
    } catch (_error) {
      // A blocked preference store must not prevent the local UI from loading.
    }
    const languages = Array.isArray(navigator.languages) ? navigator.languages : [];
    return normalize(languages[0] || navigator.language || navigator.userLanguage);
  }

  const initialLanguage = detect();
  if (!window.i18next || !window.PairRoomLocales) {
    throw new Error('PairRoom i18n assets did not load in dependency order');
  }
  window.i18next.init({
    lng: initialLanguage,
    // The catalogs are embedded in the same document. Synchronous setup keeps
    // the following deferred application scripts from briefly rendering raw
    // semantic keys before i18next's default async initialization callback.
    initAsync: false,
    fallbackLng: 'en',
    supportedLngs: ['en', 'zh-CN'],
    nonExplicitSupportedLngs: false,
    resources: window.PairRoomLocales,
    interpolation: { escapeValue: false },
    returnNull: false,
  });

  function t(key, options) {
    return window.i18next.t(key, options || {});
  }

  function apply(root = document) {
    const scope = root.nodeType === Node.ELEMENT_NODE || root.nodeType === Node.DOCUMENT_NODE ? root : document;
    const elements = [];
    if (scope.nodeType === Node.ELEMENT_NODE && scope.hasAttribute('data-i18n')) elements.push(scope);
    elements.push(...scope.querySelectorAll('[data-i18n]'));
    for (const element of elements) element.textContent = t(element.getAttribute('data-i18n'));
    for (const attr of ['title', 'aria-label', 'placeholder']) {
      const marker = `data-i18n-${attr}`;
      const targets = [];
      if (scope.nodeType === Node.ELEMENT_NODE && scope.hasAttribute(marker)) targets.push(scope);
      targets.push(...scope.querySelectorAll(`[${marker}]`));
      for (const element of targets) element.setAttribute(attr, t(element.getAttribute(marker)));
    }
    const toggle = document.getElementById('language-button');
    if (toggle) {
      const chinese = window.i18next.resolvedLanguage === 'zh-CN';
      toggle.textContent = chinese ? 'EN' : t('common.chineseShort');
      const key = chinese ? 'common.switchToEnglish' : 'common.switchToChinese';
      toggle.title = t(key);
      toggle.setAttribute('aria-label', t(key));
    }
    document.documentElement.lang = window.i18next.resolvedLanguage === 'zh-CN' ? 'zh-CN' : 'en';
  }

  async function setLang(value, persist = true) {
    const language = normalize(value);
    await window.i18next.changeLanguage(language);
    if (persist) {
      try { localStorage.setItem(STORAGE_KEY, language); } catch (_error) { /* best effort */ }
    }
    apply(document);
    document.dispatchEvent(new CustomEvent('pairroom:lang', { detail: { lang: language } }));
  }

  function locale() { return window.i18next.resolvedLanguage === 'zh-CN' ? 'zh-CN' : 'en'; }
  function formatNumber(value, options) { return new Intl.NumberFormat(locale(), options).format(value); }
  function formatDate(value, options) { return new Intl.DateTimeFormat(locale(), options).format(new Date(value)); }
  function formatRelative(value, unit) { return new Intl.RelativeTimeFormat(locale(), { numeric: 'auto' }).format(value, unit); }
	function formatList(values, options) { return new Intl.ListFormat(locale(), options || { style: 'short', type: 'conjunction' }).format(values || []); }
  function errorMessage(payload) {
	if (!payload || typeof payload !== 'object') return String(payload || '');
	const key = payload.code ? `errors.${payload.code}` : '';
	if (key && window.i18next.exists(key)) {
	  const localized = t(key, payload.params || payload.details || {});
	  const detailCodes = new Set(['invalid_request', 'request_failed', 'request_conflict', 'internal_error', 'native_runtime_error', 'service_unavailable']);
	  if (detailCodes.has(payload.code) && payload.error && String(payload.error) !== localized) return `${localized} ${payload.error}`;
	  return localized;
	}
    return String(payload.error || payload.detail || '');
  }

  window.PairRoomI18n = {
    get lang() { return locale(); },
    t, setLang, apply, formatNumber, formatDate, formatRelative, formatList, errorMessage, STORAGE_KEY,
  };

  document.addEventListener('click', (event) => {
    const button = event.target.closest('#language-button');
    if (!button) return;
    setLang(locale() === 'zh-CN' ? 'en' : 'zh-CN');
  });
  window.addEventListener('storage', (event) => {
    if (event.key === STORAGE_KEY && event.newValue) setLang(event.newValue, false);
  });
  const observer = new MutationObserver((records) => {
    for (const record of records) for (const node of record.addedNodes) if (node.nodeType === Node.ELEMENT_NODE) apply(node);
  });
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => {
      apply(document);
      observer.observe(document.body, { childList: true, subtree: true });
    }, { once: true });
  } else {
    apply(document);
    observer.observe(document.body, { childList: true, subtree: true });
  }
})();
