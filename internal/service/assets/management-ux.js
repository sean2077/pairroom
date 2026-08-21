(() => {
  'use strict';

  const app = document.getElementById('app');
  const sidebar = document.getElementById('sidebar');
  const sidebarToggle = document.getElementById('sidebar-collapse');
  const workspaceShell = document.querySelector('.workspace-shell');
  const topbar = document.querySelector('.topbar');
  const topbarActions = document.querySelector('.topbar-actions');
  const view = document.getElementById('view');
  const globalSearch = document.getElementById('global-search');
  const refreshButton = document.getElementById('refresh-button');
  const connectionBanner = document.getElementById('connection-banner');
  const addProjectButton = document.getElementById('add-project-button');

  if (!app || !sidebar || !workspaceShell || !topbar || !topbarActions || !view) return;

  let paletteOpen = false;
  let paletteItems = [];
  let activePaletteIndex = 0;
  let chord = '';
  let chordTimer = 0;
  let enhancementFrame = 0;
  let refreshDeferredWhilePaletteOpen = false;

  document.body.classList.add('management-ux-enhanced');
  installSkipLink();
  const syncLine = installSyncLine();
  const commandUI = installCommandPalette();
  const mobileNav = installMobileNavigation();
  const chordHint = installChordHint();

  improveStaticSemantics();
  enhanceDynamicContent();
  syncRouteState();
  syncSidebarState();
  syncConnectionState();
  installObservers();
  installKeyboardNavigation();
  installScrollFeedback();

  function installSkipLink() {
    if (document.querySelector('.management-skip-link')) return;
    const link = document.createElement('a');
    link.className = 'management-skip-link';
    link.href = '#view';
    link.textContent = '跳到页面内容';
    document.body.prepend(link);
  }

  function installSyncLine() {
    const line = document.createElement('div');
    line.className = 'management-sync-line';
    line.setAttribute('role', 'progressbar');
    line.setAttribute('aria-label', '正在同步 Management Shell');
    line.setAttribute('aria-hidden', 'true');
    workspaceShell.prepend(line);
    return line;
  }

  function installCommandPalette() {
    const button = document.createElement('button');
    button.id = 'management-command-button';
    button.className = 'icon-button management-command-button';
    button.type = 'button';
    button.textContent = '⌘';
    button.title = '快速操作（Ctrl/⌘ K）';
    button.setAttribute('aria-label', '打开快速操作');
    button.setAttribute('aria-keyshortcuts', 'Control+K Meta+K');
    button.setAttribute('aria-expanded', 'false');
    button.setAttribute('aria-controls', 'management-command-dialog');
    topbarActions.insertBefore(button, refreshButton || topbarActions.firstChild);

    const dialog = document.createElement('dialog');
    dialog.id = 'management-command-dialog';
    dialog.className = 'management-command-dialog';
    dialog.setAttribute('aria-labelledby', 'management-command-title');
    dialog.innerHTML = `
      <div class="management-command-shell">
        <header class="management-command-header">
          <span class="management-command-symbol" aria-hidden="true">⌕</span>
          <label class="visually-hidden" for="management-command-input" id="management-command-title">快速操作</label>
          <input id="management-command-input" type="search" autocomplete="off" placeholder="跳转、创建或执行操作…" role="combobox" aria-autocomplete="list" aria-controls="management-command-list" aria-expanded="true">
          <kbd>Esc</kbd>
        </header>
        <div id="management-command-list" class="management-command-list" role="listbox" aria-label="可用操作"></div>
        <footer class="management-command-footer">
          <span><kbd>↑</kbd><kbd>↓</kbd> 选择</span>
          <span><kbd>Enter</kbd> 执行</span>
          <span><kbd>G</kbd> + 键 导航</span>
        </footer>
      </div>`;
    document.body.append(dialog);

    const input = dialog.querySelector('#management-command-input');
    const list = dialog.querySelector('#management-command-list');

    button.addEventListener('click', () => openPalette());
    input.addEventListener('input', () => renderPalette(input.value));
    input.addEventListener('keydown', (event) => {
      if (event.key === 'ArrowDown') {
        event.preventDefault();
        movePaletteSelection(1);
      } else if (event.key === 'ArrowUp') {
        event.preventDefault();
        movePaletteSelection(-1);
      } else if (event.key === 'Home') {
        event.preventDefault();
        setPaletteSelection(0);
      } else if (event.key === 'End') {
        event.preventDefault();
        setPaletteSelection(paletteItems.length - 1);
      } else if (event.key === 'Enter') {
        event.preventDefault();
        runPaletteSelection();
      }
    });
    list.addEventListener('click', (event) => {
      const item = event.target.closest('[data-command-index]');
      if (!item) return;
      activePaletteIndex = Number(item.dataset.commandIndex);
      runPaletteSelection();
    });
    list.addEventListener('pointermove', (event) => {
      const item = event.target.closest('[data-command-index]');
      if (!item) return;
      setPaletteSelection(Number(item.dataset.commandIndex), { scroll: false });
    });
    dialog.addEventListener('click', (event) => {
      if (event.target === dialog) closePalette();
    });
    dialog.addEventListener('cancel', (event) => {
      event.preventDefault();
      closePalette();
    });
    dialog.addEventListener('close', finalizePaletteClose);

    return { button, dialog, input, list };
  }

  function commands() {
    return [
      {
        id: 'overview', group: '导航', icon: '◫', label: '打开概览', detail: 'Service、Project 与 Room 总览', keys: 'G O',
        keywords: 'overview home service 概览 首页', action: () => navigate('#/overview'),
      },
      {
        id: 'projects', group: '导航', icon: '⌂', label: '打开 Projects', detail: '管理 Project、Room 与 Bindings', keys: 'G P',
        keywords: 'project projects room workspace 项目 房间', action: () => navigate('#/projects'),
      },
      {
        id: 'runtimes', group: '导航', icon: '◎', label: '打开 Runtimes', detail: '容量、队列与活动 Turn', keys: 'G R',
        keywords: 'runtime runtimes queue capacity turn 运行时 队列', action: () => navigate('#/runtimes'),
      },
      {
        id: 'settings', group: '导航', icon: '⚙', label: '打开设置', detail: '界面、Runtime 与诊断', keys: 'G S',
        keywords: 'settings preferences diagnostics 设置 偏好 诊断', action: () => navigate('#/settings'),
      },
      {
        id: 'new-project', group: '操作', icon: '＋', label: '登记 Project', detail: '添加 canonical Git worktree', keys: 'N P',
        keywords: 'new add register project 新建 添加 登记', action: () => addProjectButton?.click(),
      },
      {
        id: 'search', group: '操作', icon: '⌕', label: '搜索 Project 或 Room', detail: '进入全局搜索', keys: '/',
        keywords: 'search find project room 搜索 查找', action: () => globalSearch?.focus({ preventScroll: true }),
      },
      {
        id: 'refresh', group: '操作', icon: '↻', label: '立即刷新', detail: '同步最新 Service snapshot', keys: '',
        keywords: 'refresh sync reload 刷新 同步', action: () => refreshButton?.click(),
      },
      {
        id: 'sidebar', group: '视图', icon: '◧', label: app.classList.contains('sidebar-collapsed') ? '展开侧边栏' : '折叠侧边栏',
        detail: '切换 Management 导航宽度', keys: '', keywords: 'sidebar collapse expand 侧边栏 折叠 展开', action: toggleSidebar,
      },
    ];
  }

  function openPalette(initialQuery = '') {
    if (app.hidden || document.querySelector('dialog[open]:not(#management-command-dialog)')) return;
    paletteOpen = true;
    activePaletteIndex = 0;
    if (refreshButton?.classList.contains('spinning')) refreshDeferredWhilePaletteOpen = true;
    commandUI.button.setAttribute('aria-expanded', 'true');
    commandUI.input.value = initialQuery;
    renderPalette(initialQuery);
    if (!commandUI.dialog.open) commandUI.dialog.showModal();
    requestAnimationFrame(() => commandUI.input.focus({ preventScroll: true }));
  }

  function closePalette() {
    if (!commandUI.dialog.open) return;
    commandUI.dialog.close();
    finalizePaletteClose();
  }

  function finalizePaletteClose() {
    if (!paletteOpen && commandUI.button.getAttribute('aria-expanded') !== 'true') return;
    paletteOpen = false;
    commandUI.button.setAttribute('aria-expanded', 'false');
    if (refreshButton?.classList.contains('spinning')) refreshDeferredWhilePaletteOpen = true;
    requestAnimationFrame(() => {
      if (!app.hidden) commandUI.button.focus({ preventScroll: true });
    });
    flushDeferredRefresh();
  }

  function flushDeferredRefresh() {
    if (!refreshDeferredWhilePaletteOpen) return;
    refreshDeferredWhilePaletteOpen = false;
    if (!app.hidden) window.dispatchEvent(new HashChangeEvent('hashchange'));
  }

  function renderPalette(query = '') {
    const terms = normalize(query).split(/\s+/).filter(Boolean);
    paletteItems = commands().filter((command) => {
      const haystack = normalize(`${command.label} ${command.detail} ${command.group} ${command.keywords}`);
      return terms.every((term) => haystack.includes(term));
    });
    activePaletteIndex = Math.min(activePaletteIndex, Math.max(0, paletteItems.length - 1));
    commandUI.list.replaceChildren();

    if (!paletteItems.length) {
      const empty = document.createElement('div');
      empty.className = 'management-command-empty';
      empty.innerHTML = '<strong>没有匹配的操作</strong><span>尝试“Project”“刷新”或“设置”。</span>';
      commandUI.list.append(empty);
      commandUI.input.removeAttribute('aria-activedescendant');
      return;
    }

    let previousGroup = '';
    paletteItems.forEach((command, index) => {
      if (command.group !== previousGroup) {
        const heading = document.createElement('div');
        heading.className = 'management-command-group';
        heading.textContent = command.group;
        heading.setAttribute('role', 'presentation');
        commandUI.list.append(heading);
        previousGroup = command.group;
      }
      const item = document.createElement('button');
      item.id = `management-command-${command.id}`;
      item.className = 'management-command-item';
      item.type = 'button';
      item.dataset.commandIndex = String(index);
      item.setAttribute('role', 'option');
      item.setAttribute('aria-selected', String(index === activePaletteIndex));
      item.innerHTML = `
        <span class="management-command-icon" aria-hidden="true">${command.icon}</span>
        <span class="management-command-copy"><strong></strong><small></small></span>
        ${command.keys ? `<kbd>${command.keys}</kbd>` : ''}`;
      item.querySelector('strong').textContent = command.label;
      item.querySelector('small').textContent = command.detail;
      commandUI.list.append(item);
    });
    syncPaletteSelection({ scroll: false });
  }

  function normalize(value) {
    return String(value || '').trim().toLocaleLowerCase();
  }

  function movePaletteSelection(delta) {
    if (!paletteItems.length) return;
    activePaletteIndex = (activePaletteIndex + delta + paletteItems.length) % paletteItems.length;
    syncPaletteSelection();
  }

  function setPaletteSelection(index, { scroll = true } = {}) {
    if (!paletteItems.length) return;
    activePaletteIndex = Math.max(0, Math.min(index, paletteItems.length - 1));
    syncPaletteSelection({ scroll });
  }

  function syncPaletteSelection({ scroll = true } = {}) {
    const items = Array.from(commandUI.list.querySelectorAll('[data-command-index]'));
    items.forEach((item, index) => item.setAttribute('aria-selected', String(index === activePaletteIndex)));
    const active = items[activePaletteIndex];
    if (!active) return;
    commandUI.input.setAttribute('aria-activedescendant', active.id);
    if (scroll) active.scrollIntoView({ block: 'nearest' });
  }

  function runPaletteSelection() {
    const command = paletteItems[activePaletteIndex];
    if (!command) return;
    closePalette();
    requestAnimationFrame(() => command.action());
  }

  function navigate(hash) {
    if (location.hash === hash) {
      view.focus({ preventScroll: true });
      window.scrollTo({ top: 0, behavior: reducedMotion() ? 'auto' : 'smooth' });
      return;
    }
    location.hash = hash;
  }

  function installMobileNavigation() {
    const nav = document.createElement('nav');
    nav.className = 'management-mobile-nav';
    nav.setAttribute('aria-label', '移动端主导航');
    nav.innerHTML = `
      <a href="#/overview" data-mobile-nav="overview"><span aria-hidden="true">◫</span><small>概览</small></a>
      <a href="#/projects" data-mobile-nav="projects"><span aria-hidden="true">⌂</span><small>Projects</small></a>
      <a href="#/runtimes" data-mobile-nav="runtimes"><span aria-hidden="true">◎</span><small>Runtimes</small></a>
      <a href="#/settings" data-mobile-nav="settings"><span aria-hidden="true">⚙</span><small>设置</small></a>`;
    app.append(nav);
    return nav;
  }

  function installChordHint() {
    const hint = document.createElement('div');
    hint.className = 'management-chord-hint';
    hint.setAttribute('role', 'status');
    hint.setAttribute('aria-live', 'polite');
    hint.hidden = true;
    app.append(hint);
    return hint;
  }

  function toggleSidebar() {
    if (window.matchMedia('(max-width: 900px)').matches) {
      document.getElementById('mobile-menu')?.click();
      return;
    }
    sidebarToggle?.click();
  }

  function syncSidebarState() {
    const collapsed = app.classList.contains('sidebar-collapsed');
    if (sidebarToggle) {
      sidebarToggle.setAttribute('aria-expanded', String(!collapsed));
      sidebarToggle.setAttribute('aria-label', collapsed ? '展开侧边栏' : '折叠侧边栏');
      sidebarToggle.title = collapsed ? '展开侧边栏' : '折叠侧边栏';
    }
  }

  function improveStaticSemantics() {
    globalSearch?.setAttribute('aria-keyshortcuts', '/');
    refreshButton?.setAttribute('aria-label', '刷新 Management Shell');
    view.setAttribute('aria-busy', 'true');
    document.querySelector('.primary-nav')?.setAttribute('aria-label', 'Management 主导航');
    document.querySelector('.workspace-footer')?.setAttribute('role', 'contentinfo');
  }

  function enhanceDynamicContent() {
    view.querySelectorAll('.project-card').forEach((card, index) => {
      const title = card.querySelector('h2')?.textContent?.trim() || `Project ${index + 1}`;
      card.setAttribute('aria-label', `Project：${title}`);
    });
    view.querySelectorAll('.room-row').forEach((row, index) => {
      const title = row.querySelector('.room-title-line strong')?.textContent?.trim() || `Room ${index + 1}`;
      row.setAttribute('role', 'group');
      row.setAttribute('aria-label', `Room：${title}`);
    });
    view.querySelectorAll('.skeleton').forEach((element) => element.setAttribute('aria-hidden', 'true'));
    view.setAttribute('aria-busy', String(Boolean(view.querySelector('.skeleton'))));
  }

  function scheduleDynamicEnhancement() {
    if (enhancementFrame) return;
    enhancementFrame = requestAnimationFrame(() => {
      enhancementFrame = 0;
      enhanceDynamicContent();
      syncRouteState();
    });
  }

  function syncRouteState() {
    const raw = location.hash.startsWith('#/') ? location.hash.slice(2) : 'overview';
    const route = raw.split('/')[0] || 'overview';
    const activeRoute = raw.startsWith('projects/') ? 'projects' : route;
    document.querySelectorAll('.primary-nav [data-nav]').forEach((link) => {
      const current = link.dataset.nav === activeRoute;
      if (current) link.setAttribute('aria-current', 'page');
      else link.removeAttribute('aria-current');
    });
    mobileNav.querySelectorAll('[data-mobile-nav]').forEach((link) => {
      const current = link.dataset.mobileNav === activeRoute;
      if (current) link.setAttribute('aria-current', 'page');
      else link.removeAttribute('aria-current');
    });
  }

  function syncConnectionState() {
    const syncing = refreshButton?.classList.contains('spinning') === true;
    const disconnected = connectionBanner ? !connectionBanner.hidden : false;
    if (paletteOpen && syncing) refreshDeferredWhilePaletteOpen = true;
    document.body.classList.toggle('management-is-syncing', syncing);
    document.body.classList.toggle('management-is-disconnected', disconnected);
    syncLine.setAttribute('aria-hidden', String(!syncing));
  }

  function installObservers() {
    const appObserver = new MutationObserver(() => {
      syncSidebarState();
      if (app.hidden && paletteOpen) closePalette();
    });
    appObserver.observe(app, { attributes: true, attributeFilter: ['class', 'hidden'] });

    const viewObserver = new MutationObserver(scheduleDynamicEnhancement);
    viewObserver.observe(view, { childList: true, subtree: true });

    const connectionObserver = new MutationObserver(syncConnectionState);
    if (refreshButton) connectionObserver.observe(refreshButton, { attributes: true, attributeFilter: ['class'] });
    if (connectionBanner) connectionObserver.observe(connectionBanner, { attributes: true, attributeFilter: ['hidden'] });

    window.addEventListener('hashchange', syncRouteState);
  }

  function installKeyboardNavigation() {
    document.addEventListener('keydown', (event) => {
      if (app.hidden) return;
      if (event.key === 'Escape' && chord) {
        event.preventDefault();
        clearChord();
        return;
      }
      const modifier = event.metaKey || event.ctrlKey;
      if (modifier && event.key.toLocaleLowerCase() === 'k') {
        event.preventDefault();
        if (paletteOpen) closePalette();
        else openPalette();
        return;
      }
      if (paletteOpen || app.hidden || document.querySelector('dialog[open]')) return;
      if (isEditableTarget(event.target) || modifier || event.altKey) return;

      const key = event.key.toLocaleLowerCase();
      if (!chord && (key === 'g' || key === 'n')) {
        event.preventDefault();
        beginChord(key);
        return;
      }
      if (!chord) return;

      event.preventDefault();
      const sequence = `${chord}${key}`;
      clearChord();
      const actions = {
        go: () => navigate('#/overview'),
        gp: () => navigate('#/projects'),
        gr: () => navigate('#/runtimes'),
        gs: () => navigate('#/settings'),
        np: () => addProjectButton?.click(),
      };
      actions[sequence]?.();
    });
  }

  function beginChord(key) {
    chord = key;
    const options = key === 'g' ? 'O 概览 · P Projects · R Runtimes · S 设置' : 'P 登记 Project';
    chordHint.textContent = `${key.toUpperCase()} → ${options}`;
    chordHint.hidden = false;
    window.clearTimeout(chordTimer);
    chordTimer = window.setTimeout(clearChord, 1600);
  }

  function clearChord() {
    chord = '';
    chordHint.hidden = true;
    window.clearTimeout(chordTimer);
  }

  function isEditableTarget(target) {
    return target instanceof HTMLElement && (target.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(target.tagName));
  }

  function installScrollFeedback() {
    const update = () => topbar.classList.toggle('management-topbar-scrolled', window.scrollY > 6);
    window.addEventListener('scroll', update, { passive: true });
    update();
  }

  function reducedMotion() {
    return window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  }
})();
