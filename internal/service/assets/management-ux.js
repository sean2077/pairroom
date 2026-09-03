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
  const roomTree = document.getElementById('room-tree');
  const roomTabstrip = document.getElementById('room-tabstrip');
  const roomTablist = document.getElementById('room-tablist');
  const roomStage = document.getElementById('room-stage');
  const roomPickerButton = document.getElementById('room-picker-button');

  if (!app || !sidebar || !workspaceShell || !topbar || !topbarActions || !view) return;

  let paletteOpen = false;
  let paletteItems = [];
  let activePaletteIndex = 0;
  let chord = '';
  let chordTimer = 0;
  let enhancementFrame = 0;
  let roomChromeFrame = 0;
  let pendingRoomTabFocusID = '';
  let pendingGlobalSearchFocus = false;
  let paletteReturnFocus = null;
  let refreshDeferredWhilePaletteOpen = false;

  document.body.classList.add('management-ux-enhanced');
  const skipLink = installSkipLink();
  const syncLine = installSyncLine();
  const commandUI = installCommandPalette();
  const mobileNav = installMobileNavigation();
  const chordHint = installChordHint();
  const roomWorkspaceUI = installRoomWorkspaceTools();

  improveStaticSemantics();
  enhanceDynamicContent();
  enhanceRoomChrome();
  syncRouteState();
  syncSidebarState();
  syncConnectionState();
  installObservers();
  installRoomWorkspaceShortcuts();
  installKeyboardNavigation();
  installRoomTabNavigation();
  installScrollFeedback();

  function installSkipLink() {
    const existing = document.querySelector('.management-skip-link');
    if (existing) return existing;
    const link = document.createElement('a');
    link.className = 'management-skip-link';
    link.href = '#view';
    link.textContent = '跳到页面内容';
    link.addEventListener('click', activateSkipLink);
    document.body.prepend(link);
    return link;
  }

  function activateSkipLink(event) {
    // The shell routes on location.hash. Native anchor navigation would rewrite the
    // hash (e.g. to #room-stage) and the router would interpret that as a route
    // change, dropping the user out of the Room. Move focus to the target directly.
    event.preventDefault();
    const target = document.querySelector(skipLink.getAttribute('href'));
    if (!(target instanceof HTMLElement)) return;
    target.focus({ preventScroll: true });
    target.scrollIntoView({ block: 'start', behavior: reducedMotion() ? 'auto' : 'smooth' });
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
    const items = [
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
        id: 'open-room', group: '操作', icon: '◇', label: '打开 Room 标签', detail: '在应用内标签中打开一个活动 Room', keys: 'Alt+N',
        keywords: 'open room tab 打开 标签', action: () => roomPickerButton?.click(),
      },
      {
        id: 'search', group: '操作', icon: '⌕', label: '搜索 Project 或 Room', detail: '进入全局搜索', keys: '/',
        keywords: 'search find project room 搜索 查找', action: focusGlobalSearch,
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
    const currentTab = currentRoomTab();
    if (currentTab) {
      items.splice(6, 0, {
        id: 'close-room', group: '标签', icon: '×', label: '关闭当前 Room 标签', detail: '仅关闭工作台标签，不停止 Agent', keys: 'Alt+W',
        keywords: 'close room tab 关闭 标签', action: () => currentTab.querySelector('.room-tab-close')?.click(),
      });
    }
    return items;
  }

  function openPalette(initialQuery = '') {
    if (app.hidden || document.querySelector('dialog[open]:not(#management-command-dialog)')) return;
    const activeElement = document.activeElement;
    paletteReturnFocus = activeElement instanceof HTMLElement && activeElement !== document.body ? activeElement : null;
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
    const returnFocus = paletteReturnFocus;
    paletteReturnFocus = null;
    requestAnimationFrame(() => {
      if (app.hidden) return;
      const fallback = app.classList.contains('room-workspace') ? roomWorkspaceUI.command : commandUI.button;
      const target = returnFocus?.isConnected ? returnFocus : fallback;
      target?.focus({ preventScroll: true });
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

  function focusGlobalSearch() {
    if (app.classList.contains('room-workspace')) {
      pendingGlobalSearchFocus = true;
      navigate('#/projects');
      return;
    }
    globalSearch?.focus({ preventScroll: true });
  }

  function navigate(hash) {
    if (location.hash === hash) {
      const target = app.classList.contains('room-workspace') ? roomStage : view;
      target?.focus({ preventScroll: true });
      if (!app.classList.contains('room-workspace')) {
        window.scrollTo({ top: 0, behavior: reducedMotion() ? 'auto' : 'smooth' });
      }
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

  function makeRoomTool({ className = '', label, title, text, onClick }) {
    const button = document.createElement('button');
    button.className = `icon-button room-workspace-tool ${className}`.trim();
    button.type = 'button';
    button.textContent = text;
    button.title = title || label;
    button.setAttribute('aria-label', label);
    button.addEventListener('click', onClick);
    return button;
  }

  function installRoomWorkspaceTools() {
    if (!roomTabstrip || !roomTablist) return {};

    const leading = document.createElement('div');
    leading.className = 'room-workspace-leading';
    const menu = makeRoomTool({
      className: 'room-workspace-menu',
      label: '打开导航',
      title: '打开 Projects 与 Rooms 导航',
      text: '☰',
      onClick: () => document.getElementById('mobile-menu')?.click(),
    });
    leading.append(menu);
    roomTabstrip.insertBefore(leading, roomTablist);

    const tools = document.createElement('div');
    tools.className = 'room-workspace-tools';
    const refresh = makeRoomTool({
      className: 'room-workspace-refresh',
      label: '刷新 Management Shell',
      title: '同步 Service 与 Runtime 状态',
      text: '↻',
      onClick: () => refreshButton?.click(),
    });
    const command = makeRoomTool({
      className: 'room-workspace-command',
      label: '打开快速操作',
      title: '快速操作（Ctrl/⌘ K）',
      text: '⌘',
      onClick: () => openPalette(),
    });
    command.setAttribute('aria-keyshortcuts', 'Control+K Meta+K');
    if (roomPickerButton) {
      roomPickerButton.classList.add('room-workspace-picker');
      roomPickerButton.title = '打开 Room 标签（Alt+N）';
      roomPickerButton.setAttribute('aria-label', '打开 Room 标签');
    }
    tools.append(refresh, command);
    roomTabstrip.append(tools);
    return { leading, menu, tools, refresh, command };
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
    roomTablist?.setAttribute('aria-orientation', 'horizontal');
    roomTabstrip?.setAttribute('aria-label', 'Room 工作区标签');
    roomStage?.setAttribute('tabindex', '-1');
    roomStage?.setAttribute('aria-label', '当前 Room 工作区');
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

  function scheduleRoomChromeEnhancement() {
    if (roomChromeFrame) return;
    roomChromeFrame = requestAnimationFrame(() => {
      roomChromeFrame = 0;
      enhanceRoomChrome();
    });
  }

  function enhanceRoomChrome() {
    enhanceRoomTree();
    enhanceRoomTabs();
    syncRoomTabOverflow();
  }

  function enhanceRoomTree() {
    if (!roomTree) return;
    roomTree.querySelectorAll('.tree-project').forEach((section) => {
      const toggle = section.querySelector('.tree-project-toggle');
      if (!toggle) return;
      toggle.setAttribute('aria-expanded', String(section.classList.contains('open')));
    });
    roomTree.querySelectorAll('.tree-room').forEach((button) => {
      if (button.classList.contains('active')) button.setAttribute('aria-current', 'page');
      else button.removeAttribute('aria-current');
    });
  }

  function directChildByClass(parent, className) {
    return Array.from(parent?.children || []).find((child) => child.classList?.contains(className)) || null;
  }

  function roomDOMToken(roomID) {
    return encodeURIComponent(String(roomID || 'room')).replace(/%/g, '_');
  }

  function findRoomPanel(roomID) {
    return Array.from(roomStage?.children || []).find((panel) => panel.dataset?.roomId === roomID) || null;
  }

  function currentRoomTab() {
    return roomTablist?.querySelector('.room-tab.active') || null;
  }

  function currentRoomTabTargets() {
    return Array.from(roomTablist?.querySelectorAll('.room-tab-target') || []);
  }

  function findRoomTabTarget(roomID) {
    return currentRoomTabTargets().find((target) => target.closest('.room-tab')?.dataset.roomId === roomID) || null;
  }

  function rememberAdjacentRoomTabFocus(tab) {
    const tabs = Array.from(roomTablist?.querySelectorAll('.room-tab') || []);
    const index = tabs.indexOf(tab);
    const active = tabs.find((candidate) => candidate !== tab && candidate.classList.contains('active'));
    const target = tab.classList.contains('active') && index >= 0 ? (tabs[index + 1] || tabs[index - 1]) : active;
    pendingRoomTabFocusID = target?.dataset.roomId || '';
  }

  function enhanceRoomTabs() {
    if (!roomTablist) return;
    const tabs = Array.from(roomTablist.children).filter((child) => child.classList?.contains('room-tab'));
    tabs.forEach((tab) => {
      const roomID = tab.dataset.roomId || '';
      const selected = tab.classList.contains('active') || tab.getAttribute('aria-selected') === 'true';
      const close = directChildByClass(tab, 'room-tab-close') || tab.querySelector('.room-tab-close');
      let target = directChildByClass(tab, 'room-tab-target');

      if (!target) {
        target = document.createElement('button');
        target.type = 'button';
        target.className = 'room-tab-target';
        target.setAttribute('role', 'tab');
        target.draggable = true;
        const content = Array.from(tab.childNodes).filter((child) => child !== close);
        content.forEach((child) => target.append(child));
        tab.insertBefore(target, close || null);
        tab.setAttribute('role', 'presentation');
        tab.removeAttribute('aria-selected');
        tab.removeAttribute('tabindex');
        tab.draggable = false;
        target.addEventListener('keydown', handleRoomTabKeydown);
        target.addEventListener('auxclick', (event) => {
          if (event.button !== 1) return;
          event.preventDefault();
          close?.click();
        });
        target.addEventListener('dragstart', () => tab.classList.add('dragging'));
        target.addEventListener('dragend', () => tab.classList.remove('dragging'));
        close?.addEventListener('pointerdown', (event) => event.stopPropagation());
        close?.addEventListener('click', () => rememberAdjacentRoomTabFocus(tab), { capture: true });
        close?.addEventListener('dragstart', (event) => event.preventDefault());
      }

      const label = target.querySelector('.room-tab-label')?.textContent?.trim() || roomID || 'Room';
      const token = roomDOMToken(roomID);
      const panel = findRoomPanel(roomID);
      target.id = `room-tab-${token}`;
      target.setAttribute('aria-selected', String(selected));
      target.setAttribute('aria-label', `${label}${selected ? '，当前标签' : ''}`);
      target.setAttribute('aria-keyshortcuts', 'Delete');
      target.tabIndex = selected ? 0 : -1;
      target.title = label;
      if (panel) {
        panel.id = `room-panel-${token}`;
        panel.setAttribute('aria-labelledby', target.id);
        target.setAttribute('aria-controls', panel.id);
      }
      if (close) {
        close.draggable = false;
        close.tabIndex = selected ? 0 : -1;
        close.title = `关闭 ${label}（不停止 Agent）`;
        close.setAttribute('aria-label', `关闭 ${label}；不停止 Agent`);
        close.setAttribute('aria-keyshortcuts', 'Alt+W');
      }
    });

    const active = tabs.find((tab) => tab.classList.contains('active'));
    if (active || pendingRoomTabFocusID) {
      requestAnimationFrame(() => {
        active?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
        const focusRoomID = pendingRoomTabFocusID;
        if (focusRoomID) {
          const focusTarget = findRoomTabTarget(focusRoomID) || active?.querySelector('.room-tab-target');
          focusTarget?.focus({ preventScroll: true });
          if (pendingRoomTabFocusID === focusRoomID) pendingRoomTabFocusID = '';
        }
        syncRoomTabOverflow();
      });
    }
  }

  function handleRoomTabKeydown(event) {
    const targets = currentRoomTabTargets();
    const current = event.currentTarget;
    const index = targets.indexOf(current);
    if (index < 0) return;

    if (event.key === 'Delete') {
      event.preventDefault();
      current.closest('.room-tab')?.querySelector('.room-tab-close')?.click();
      return;
    }

    let nextIndex = -1;
    if (event.key === 'ArrowLeft') nextIndex = (index - 1 + targets.length) % targets.length;
    else if (event.key === 'ArrowRight') nextIndex = (index + 1) % targets.length;
    else if (event.key === 'Home') nextIndex = 0;
    else if (event.key === 'End') nextIndex = targets.length - 1;
    if (nextIndex < 0) return;

    event.preventDefault();
    const next = targets[nextIndex];
    const roomID = next.closest('.room-tab')?.dataset.roomId || '';
    pendingRoomTabFocusID = roomID;
    next.click();
  }

  function syncRoomTabOverflow() {
    if (!roomTabstrip || !roomTablist) return;
    const max = Math.max(0, roomTablist.scrollWidth - roomTablist.clientWidth);
    const overflowing = max > 2;
    roomTabstrip.classList.toggle('room-tabs-overflowing', overflowing);
    roomTabstrip.classList.toggle('room-tabs-at-start', !overflowing || roomTablist.scrollLeft <= 2);
    roomTabstrip.classList.toggle('room-tabs-at-end', !overflowing || roomTablist.scrollLeft >= max - 2);
  }

  function syncRouteState() {
    const raw = location.hash.startsWith('#/') ? location.hash.slice(2) : 'overview';
    const route = raw.split('/')[0] || 'overview';
    const activeRoute = raw.startsWith('projects/') ? 'projects' : route;
    const roomWorkspace = raw.startsWith('rooms/') && !app.hidden;
    app.classList.toggle('room-workspace', roomWorkspace);
    document.body.classList.toggle('management-room-workspace', roomWorkspace);
    if (skipLink) {
      skipLink.hidden = app.hidden;
      skipLink.href = roomWorkspace ? '#room-stage' : '#view';
      skipLink.textContent = roomWorkspace ? '跳到当前 Room' : '跳到页面内容';
    }
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
    if (pendingGlobalSearchFocus && !roomWorkspace && !app.hidden) {
      pendingGlobalSearchFocus = false;
      requestAnimationFrame(() => globalSearch?.focus({ preventScroll: true }));
    }
    scheduleRoomChromeEnhancement();
  }

  function syncConnectionState() {
    const syncing = refreshButton?.classList.contains('spinning') === true;
    const disconnected = connectionBanner ? !connectionBanner.hidden : false;
    if (paletteOpen && syncing) refreshDeferredWhilePaletteOpen = true;
    document.body.classList.toggle('management-is-syncing', syncing);
    document.body.classList.toggle('management-is-disconnected', disconnected);
    syncLine.setAttribute('aria-hidden', String(!syncing));
    if (roomWorkspaceUI.refresh) {
      roomWorkspaceUI.refresh.classList.toggle('spinning', syncing);
      roomWorkspaceUI.refresh.disabled = Boolean(refreshButton?.disabled);
    }
  }

  function installObservers() {
    const appObserver = new MutationObserver(() => {
      syncSidebarState();
      syncRouteState();
      if (app.hidden && paletteOpen) closePalette();
    });
    appObserver.observe(app, { attributes: true, attributeFilter: ['class', 'hidden'] });

    const viewObserver = new MutationObserver(scheduleDynamicEnhancement);
    viewObserver.observe(view, { childList: true, subtree: true });

    const roomChromeObserver = new MutationObserver(scheduleRoomChromeEnhancement);
    if (roomTree) roomChromeObserver.observe(roomTree, { childList: true });
    if (roomTablist) roomChromeObserver.observe(roomTablist, { childList: true });
    if (roomStage) roomChromeObserver.observe(roomStage, { childList: true });

    const connectionObserver = new MutationObserver(syncConnectionState);
    if (refreshButton) connectionObserver.observe(refreshButton, { attributes: true, attributeFilter: ['class', 'disabled'] });
    if (connectionBanner) connectionObserver.observe(connectionBanner, { attributes: true, attributeFilter: ['hidden'] });

    if (typeof ResizeObserver === 'function' && roomTablist) {
      const resizeObserver = new ResizeObserver(syncRoomTabOverflow);
      resizeObserver.observe(roomTablist);
    } else {
      window.addEventListener('resize', syncRoomTabOverflow, { passive: true });
    }

    window.addEventListener('pairroom:management-render-pending', () => {
      if (paletteOpen) refreshDeferredWhilePaletteOpen = true;
    });
    window.addEventListener('hashchange', syncRouteState);
  }

  function installRoomTabNavigation() {
    if (!roomTablist) return;
    roomTablist.addEventListener('scroll', syncRoomTabOverflow, { passive: true });
    roomTablist.addEventListener('wheel', (event) => {
      const max = roomTablist.scrollWidth - roomTablist.clientWidth;
      if (max <= 2 || Math.abs(event.deltaY) <= Math.abs(event.deltaX)) return;
      event.preventDefault();
      roomTablist.scrollLeft += event.deltaY;
    }, { passive: false });
  }

  function installRoomWorkspaceShortcuts() {
    document.addEventListener('keydown', (event) => {
      if (app.hidden || document.querySelector('dialog[open]')) return;
      const key = event.key.toLocaleLowerCase();
      const modifier = event.metaKey || event.ctrlKey;
      if (!modifier && !event.altKey && key === '/' && app.classList.contains('room-workspace') && !isEditableTarget(event.target)) {
        event.preventDefault();
        event.stopImmediatePropagation();
        focusGlobalSearch();
        return;
      }
      if (modifier || !event.altKey) return;
      const active = currentRoomTab();
      if (!active) return;
      const tabs = Array.from(roomTablist?.querySelectorAll('.room-tab') || []);
      const index = tabs.indexOf(active);
      if (index < 0) return;
      if (key === 'w') {
        rememberAdjacentRoomTabFocus(active);
        return;
      }
      if (!event.shiftKey && (event.key === '[' || event.key === ']')) {
        const delta = event.key === '[' ? -1 : 1;
        const next = tabs[(index + delta + tabs.length) % tabs.length];
        pendingRoomTabFocusID = next?.dataset.roomId || '';
        return;
      }
      if (event.key === '{' || event.key === '}' || (event.shiftKey && (event.key === '[' || event.key === ']'))) {
        pendingRoomTabFocusID = active.dataset.roomId || '';
      }
    }, { capture: true });
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
