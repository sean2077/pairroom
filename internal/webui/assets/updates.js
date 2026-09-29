/* Optional management chrome. Never reads Room state or conversation content. */
(() => {
  'use strict';
  const AUTO_KEY = 'pairroom.updates.auto';
  const IGNORE_KEY = 'pairroom.updates.ignored';
  const HOUR = 60 * 60 * 1000;
  const STABLE = /^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/;
  const RELEASE_PREFIX = 'https://github.com/sean2077/pairroom/releases/tag/';

  function validate(value) {
    if (!value || !['available', 'current', 'unavailable', 'unsupported'].includes(value.status)
      || typeof value.current_version !== 'string' || value.current_version.length > 128) throw new Error('invalid_update_response');
    const result = { status: value.status, current_version: value.current_version };
    if (['available', 'current'].includes(value.status)) {
      if (!STABLE.test(value.current_version) || typeof value.latest_version !== 'string' || value.latest_version.length > 128 || !STABLE.test(value.latest_version)) throw new Error('invalid_update_response');
      const link = new URL(value.release_url);
      const tag = decodeURIComponent(link.pathname.slice('/sean2077/pairroom/releases/tag/'.length));
      if (!link.href.startsWith(RELEASE_PREFIX) || link.search || link.hash || !STABLE.test(tag)
        || tag.replace(/^v/, '') !== value.latest_version.replace(/^v/, '')) throw new Error('invalid_update_response');
      result.latest_version = value.latest_version;
      result.release_url = link.href;
    }
    for (const key of ['checked_at', 'next_check_at']) {
      if (value[key]) {
        if (typeof value[key] !== 'string' || !Number.isFinite(Date.parse(value[key]))) throw new Error('invalid_update_response');
        result[key] = value[key];
      }
    }
    return result;
  }

  // The controller owns cancellation and local UI preferences. The authenticated
  // host owns release caching, so multiple windows do not fan out to GitHub.
  function create({ request, onChange = () => {}, storage, now = Date.now }) {
    let preferences = { auto: false, ignored: '' };
    let active = false, result = null, failed = false, pending = null, nextAutomatic = 0;
    function readPreferences() {
      try {
        if (storage) preferences = { auto: storage.getItem(AUTO_KEY) === 'true', ignored: storage.getItem(IGNORE_KEY) || '' };
      } catch (_) { /* Keep in-memory preferences when storage is blocked. */ }
    }
    readPreferences();
    function save(key, value) { try { storage?.setItem(key, value); } catch (_) { /* best effort */ } }
    function state() {
      return { ...preferences, active, result, failed, checking: Boolean(pending),
        notify: active && preferences.auto && result?.status === 'available' && result.latest_version !== preferences.ignored };
    }
    function emit() { onChange(state()); }
    function cancel() {
      const old = pending;
      pending = null;
      old?.controller.abort();
    }
    function check(manual = false) {
      if (!active || (!manual && (!preferences.auto || now() < nextAutomatic))) return Promise.resolve();
      if (pending) return pending.promise;
      const ticket = { controller: new AbortController(), manual, promise: null };
      pending = ticket;
      failed = false;
      emit();
      ticket.promise = (async () => {
        const timeout = setTimeout(() => ticket.controller.abort(), 10000);
        try {
          const value = validate(await request({ manual, signal: ticket.controller.signal }));
          if (pending !== ticket || !active) return;
          failed = value.status === 'unavailable';
          // A temporary outage must not erase a previously known newer release.
          if (!failed || !result) result = value;
          const next = Date.parse(value.next_check_at || '');
          nextAutomatic = Number.isFinite(next)
            ? Math.max(now() + 60000, Math.min(next, now() + 24 * HOUR))
            : now() + (failed ? HOUR / 4 : 6 * HOUR);
        } catch (error) {
          if (pending !== ticket || !active) return;
          if (error.status === 401 || error.status === 403) { setActive(false); return; }
          failed = true;
          nextAutomatic = now() + HOUR / 4;
        } finally {
          clearTimeout(timeout);
          if (pending === ticket) { pending = null; emit(); }
        }
      })();
      return ticket.promise;
    }
    function setActive(value) {
      const changed = active !== Boolean(value);
      active = Boolean(value);
      if (!active) { cancel(); result = null; failed = false; nextAutomatic = 0; }
      emit();
      if (active && changed) void check();
    }
    function setAuto(value) {
      preferences.auto = Boolean(value);
      save(AUTO_KEY, String(preferences.auto));
      if (!preferences.auto && pending && !pending.manual) cancel();
      emit();
      if (preferences.auto) void check();
    }
    function ignore() {
      if (result?.status !== 'available') return;
      preferences.ignored = result.latest_version;
      save(IGNORE_KEY, preferences.ignored);
      emit();
    }
    function syncPreferences() {
      readPreferences();
      if (!preferences.auto && pending && !pending.manual) cancel();
      emit();
      if (preferences.auto) void check();
    }
    return { state, check, setActive, setAuto, ignore, syncPreferences };
  }
  window.PairRoomUpdates = { create };

  const messages = {
    en: {
      title: 'Software updates', check: 'Check for updates', checking: 'Checking for updates…',
      available: 'A newer PairRoom release is available', current: 'No newer stable release is available',
      unavailable: 'Unable to check for updates. Try again later.', unsupported: 'This build has no comparable release version.',
      unchecked: 'Check the latest stable release on GitHub.', version: 'Running Service', latest: 'Latest stable release',
      lastCheck: 'Last check', auto: 'Automatically check for updates', ignore: 'Ignore this version', ignored: 'This version is ignored for automatic reminders.',
      view: 'View release notes and downloads', close: 'Close',
      privacy: 'Checks request public release metadata from GitHub through your local Service. No tokens, workspace paths, or conversation content are sent. Automatic checks are off until enabled; this preference applies to this browser.',
      boundary: 'Read the release notes before upgrading. Update through your existing installation method, then restart the owning Desktop or Service when ready. Nothing is downloaded, installed, or restarted automatically.',
    },
    'zh-CN': {
      title: '软件更新', check: '检查更新', checking: '正在检查更新…',
      available: '发现 PairRoom 新版本', current: '暂无更新的稳定版本',
      unavailable: '暂时无法检查更新，请稍后重试。', unsupported: '此构建没有可比较的发布版本。',
      unchecked: '检查 GitHub 上最新的稳定版本。', version: '运行中的 Service', latest: '最新稳定版本',
      lastCheck: '最近检查', auto: '自动检查更新', ignore: '忽略此版本', ignored: '已忽略此版本的自动提醒。',
      view: '查看发布说明和下载', close: '关闭',
      privacy: '通过本地 Service 向 GitHub 请求公开发布信息，不发送令牌、工作区路径或对话内容。自动检查默认关闭，此偏好仅适用于当前浏览器。',
      boundary: '升级前请阅读发布说明。请沿用现有安装方式更新，并在合适时机重启所属 Desktop 或 Service。不会自动下载、安装或重启。',
    },
  };

  function mount() {
    // Desktop already owns a native, opt-in checker and Settings/tray notices.
    // Never bypass its saved choice or start a second checker in its WebView.
    if (window.PairRoomDesktop || !document.body?.classList.contains('workbench-management')) return;
    const app = document.getElementById('app');
    const version = document.getElementById('sidebar-version');
    const anchor = document.getElementById('cli-build-banner');
    if (!app || !version || !anchor || !window.PairRoomI18n || !window.i18next) return;
    for (const [language, value] of Object.entries(messages)) window.i18next.addResourceBundle(language, 'translation', { updates: value }, true, true);
    const t = key => window.PairRoomI18n.t(`updates.${key}`);
    function node(tag, className = '', text = '') {
      const element = document.createElement(tag);
      element.className = className;
      element.textContent = text;
      return element;
    }
    function button(className, action) {
      const value = node('button', className);
      value.type = 'button';
      value.addEventListener('click', action);
      return value;
    }
    function releaseLink() {
      const value = node('a', 'secondary-button compact-button');
      value.target = '_blank';
      value.rel = 'noopener noreferrer';
      return value;
    }
    // Preserve the existing version node: the management renderer owns its text.
    const entry = button('text-button', () => dialog.showModal());
    entry.id = 'update-settings-button';
    version.before(entry);
    entry.append(version);

    const banner = node('section', 'cli-build-banner');
    banner.id = 'update-banner';
    banner.hidden = true;
    banner.setAttribute('role', 'status');
    banner.setAttribute('aria-live', 'polite');
    const bannerTitle = node('strong');
    const bannerActions = node('div', 'section-actions');
    const bannerLink = releaseLink();
    const bannerIgnore = button('text-button', () => { controller.ignore(); entry.focus(); });
    bannerActions.append(bannerLink, bannerIgnore);
    banner.append(bannerTitle, bannerActions);
    anchor.before(banner);

    const dialog = node('dialog', 'modal modal-medium');
    dialog.id = 'update-dialog';
    dialog.setAttribute('aria-labelledby', 'update-title');
    const header = node('header', 'modal-header');
    const title = node('h2');
    title.id = 'update-title';
    const close = button('icon-button modal-close', () => dialog.close());
    close.textContent = '×';
    header.append(title, close);
    const body = node('div', 'modal-body');
    const status = node('p');
    status.setAttribute('role', 'status');
    status.setAttribute('aria-live', 'polite');
    const details = node('p', 'muted');
    const checked = node('p', 'muted');
    const automaticLabel = node('label');
    const automatic = node('input');
    automatic.type = 'checkbox';
    automatic.addEventListener('change', () => controller.setAuto(automatic.checked));
    const automaticText = node('span');
    automaticLabel.append(automatic, automaticText);
    const privacy = node('p', 'muted');
    const boundary = node('p', 'muted');
    const ignored = node('p', 'muted');
    body.append(status, details, checked, automaticLabel, privacy, boundary, ignored);
    const footer = node('footer', 'modal-footer');
    const manual = button('secondary-button', () => controller.check(true));
    const link = releaseLink();
    const ignore = button('text-button', () => { controller.ignore(); manual.focus(); });
    footer.append(manual, link, ignore);
    dialog.append(header, body, footer);
    document.body.append(dialog);
    let storage;
    try { storage = window.localStorage; } catch (_) { /* memory-only preferences */ }

    function paint(state) {
      if (!state.active && dialog.open) dialog.close();
      const value = state.result;
      entry.title = t('title');
      entry.setAttribute('aria-label', t('title'));
      title.textContent = t('title');
      close.setAttribute('aria-label', t('close'));
      status.textContent = t(state.checking ? 'checking' : state.failed ? 'unavailable' : value?.status || 'unchecked');
      details.textContent = value ? `${t('version')}: ${value.current_version}${value.latest_version ? ` · ${t('latest')}: ${value.latest_version}` : ''}` : '';
      checked.textContent = value?.checked_at ? `${t('lastCheck')}: ${window.PairRoomI18n.formatDate(value.checked_at, { dateStyle: 'medium', timeStyle: 'short' })}` : '';
      automatic.checked = state.auto;
      automaticText.textContent = ` ${t('auto')}`;
      privacy.textContent = t('privacy');
      boundary.textContent = t('boundary');
      manual.textContent = t(state.checking ? 'checking' : 'check');
      manual.disabled = state.checking;
      const available = value?.status === 'available';
      const isIgnored = available && value.latest_version === state.ignored;
      ignored.hidden = !isIgnored;
      ignored.textContent = t('ignored');
      ignore.textContent = bannerIgnore.textContent = t('ignore');
      ignore.hidden = !available || isIgnored;
      for (const element of [link, bannerLink]) {
        element.textContent = t('view');
        element.hidden = !available;
        if (available) element.href = value.release_url;
        else element.removeAttribute('href');
      }
      bannerTitle.textContent = available ? `${t('available')} · v${value.latest_version.replace(/^v/, '')}` : '';
      banner.hidden = !state.notify;
    }
    const controller = create({ storage, onChange: paint, request: async ({ manual: force, signal }) => {
      const response = await fetch(`/api/v1/updates${force ? '?refresh=1' : ''}`, {
        signal, credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' },
      });
      if (!response.ok) throw Object.assign(new Error('update_check_failed'), { status: response.status });
      return response.json();
    } });
    let timer;
    function sync() {
      controller.setActive(!app.hidden);
      if (!timer) timer = setInterval(() => { if (!document.hidden) void controller.check(); }, HOUR / 4);
    }
    new MutationObserver(sync).observe(app, { attributes: true, attributeFilter: ['hidden'] });
    document.addEventListener('visibilitychange', () => { if (!document.hidden) void controller.check(); });
    document.addEventListener('pairroom:lang', () => paint(controller.state()));
    window.addEventListener('storage', event => { if (event.key === null || [AUTO_KEY, IGNORE_KEY].includes(event.key)) controller.syncPreferences(); });
    window.addEventListener('pagehide', () => { clearInterval(timer); timer = null; controller.setActive(false); });
    window.addEventListener('pageshow', sync);
    sync();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', mount, { once: true });
  else mount();
})();
