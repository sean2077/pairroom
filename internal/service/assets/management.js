(() => {
  'use strict';

  const INITIAL_ROUTE = '#/overview';
  const NEW_BINDING_HINT = 'materializes on first turn';
  const MAX_ROOM_BATCH_SIZE = 100;
  const hashParams = new URLSearchParams(location.hash.replace(/^#/, ''));
  let bootstrapToken = hashParams.get('token') || '';
  if (bootstrapToken) {
    history.replaceState(null, '', `${location.pathname}${location.search}${INITIAL_ROUTE}`);
  } else if (!location.hash.startsWith('#/')) {
    history.replaceState(null, '', `${location.pathname}${location.search}${INITIAL_ROUTE}`);
  }

  const state = {
    snapshot: null,
    route: parseRoute(),
    connected: false,
    authenticated: false,
    lastError: '',
    search: '',
    refreshPromise: null,
    refreshTimer: null,
    renderPending: false,
    renderedSnapshotKey: '',
    csrfToken: '',
    tabs: [],
    tabMeta: {},
    activating: new Set(),
    expandedProjects: new Set(),
    archivedOpen: new Set(),
    dragTabID: '',
    projectMode: 'register',
    bindingRoomID: '',
    confirmAction: null,
    confirmRequirement: '',
    confirmAcknowledgementRequired: false,
    selectedRoomIDs: new Set(),
    settingsSection: 'interface',
    showRawSnapshot: false,
    filters: {
      projectAvailability: 'all',
      showArchived: false,
      runtimePhase: 'all',
    },
    preferences: {
      theme: 'system',
      density: 'comfortable',
      refreshMs: 10000,
    },
  };

  const $ = (id) => document.getElementById(id);
  const app = $('app');
  const view = $('view');

  async function createBrowserSession(credential = '') {
    const token = String(credential || '').trim();
    const headers = new Headers();
    const method = token ? 'POST' : 'GET';
    if (token) headers.set('Authorization', `Bearer ${token}`);
    const response = await fetch('/api/v1/session', { method, headers, credentials: 'same-origin' });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) {
      const message = response.status === 401
        ? (token
          ? 'Service Token 无效，请检查后重试。'
          : '尚未登录。请输入 Service Token，或运行 pairroom daemon open。')
        : (payload.error || response.statusText || `HTTP ${response.status}`);
      const error = new Error(message);
      error.status = response.status;
      if (response.status === 401) error.code = token ? 'invalid_credential' : 'login_required';
      throw error;
    }
    state.csrfToken = payload.csrf_token || '';
    if (!state.csrfToken) throw new Error('Management 会话未返回 CSRF 凭证，请重新登录。');
  }

  async function initializeSession() {
    const credential = bootstrapToken;
    bootstrapToken = '';
    await createBrowserSession(credential);
  }

  function credentialFromInput(value) {
    const raw = String(value || '').trim();
    if (!raw) return '';
    if (raw.startsWith('#')) {
      return new URLSearchParams(raw.replace(/^#/, '')).get('token')?.trim() || '';
    }
    if (!/^https?:\/\//i.test(raw)) return raw;
    try {
      const url = new URL(raw);
      return new URLSearchParams(url.hash.replace(/^#/, '')).get('token')?.trim() || '';
    } catch {
      return '';
    }
  }

  function setCredentialVisibility(visible) {
    const input = $('login-token');
    const button = $('login-token-toggle');
    input.type = visible ? 'text' : 'password';
    button.textContent = visible ? '隐藏' : '显示';
    button.setAttribute('aria-pressed', String(visible));
    button.setAttribute('aria-label', visible ? '隐藏 Service Token' : '显示 Service Token');
  }

  function toggleCredentialVisibility() {
    setCredentialVisibility($('login-token').type === 'password');
    $('login-token').focus({ preventScroll: true });
  }

  function showCredentialLogin(message = '') {
    state.authenticated = false;
    state.connected = false;
    state.snapshot = null;
    state.csrfToken = '';
    state.lastError = message;
    state.renderPending = false;
    state.renderedSnapshotKey = '';
    state.search = '';
    state.selectedRoomIDs.clear();
    state.tabs = [];
    state.tabMeta = {};
    state.activating.clear();
    $('global-search').value = '';
    view.replaceChildren();
    $('room-tree')?.replaceChildren();
    $('room-tablist')?.replaceChildren();
    $('room-stage')?.replaceChildren();
    if ($('room-tabstrip')) $('room-tabstrip').hidden = true;
    if ($('room-stage')) $('room-stage').hidden = true;
    if ($('view')) $('view').hidden = false;
    $('toasts').replaceChildren();
    document.querySelectorAll('dialog[open]').forEach((dialog) => dialog.close());
    document.querySelectorAll('dialog form').forEach((form) => form.reset());
    app.classList.remove('sidebar-open');
    app.hidden = true;
    $('login-screen').hidden = false;
    $('login-pending').hidden = true;
    $('login-form').hidden = false;
    setCredentialVisibility(false);
    if (message) showFormError('login-error', message);
    else hideFormError('login-error');
    document.title = '登录 · PairRoom';
    requestAnimationFrame(() => $('login-token').focus({ preventScroll: true }));
  }

  function showManagementShell() {
    state.authenticated = true;
    $('login-screen').hidden = true;
    app.hidden = false;
    hideFormError('login-error');
  }

  async function submitCredentialLogin(event) {
    event.preventDefault();
    const raw = $('login-token').value;
    const credential = credentialFromInput(raw);
    if (!credential) {
      showFormError('login-error', raw.trim()
        ? '完整 Management URL 中未找到 #token=…。'
        : '请输入 Service Token 或完整 Management URL。');
      $('login-token').focus();
      return;
    }
    hideFormError('login-error');
    await withBusy($('login-submit'), async () => {
      try {
        await createBrowserSession(credential);
        $('login-token').value = '';
        showManagementShell();
        renderLoading();
        await refresh({ forceRender: true });
      } catch (error) {
        showCredentialLogin(error.message);
      }
    });
  }

  async function logoutBrowserSession() {
    await withBusy($('logout-button'), async () => {
      try {
        await api('/api/v1/session', { method: 'DELETE' });
        $('login-token').value = '';
        showCredentialLogin();
      } catch (error) {
        if (error.status === 401) {
          showCredentialLogin();
          return;
        }
        toast('退出失败', error.message, 'error');
      }
    });
  }

  async function connect(options = {}) {
    try {
      if (!state.csrfToken) await initializeSession();
      showManagementShell();
      return await refresh(options);
    } catch (error) {
      const message = error.code === 'login_required' ? '' : error.message;
      showCredentialLogin(message);
      if (options.notify && message) toast('连接失败', message, 'error');
      return null;
    }
  }

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    const method = String(options.method || 'GET').toUpperCase();
    if (!['GET', 'HEAD', 'OPTIONS'].includes(method) && state.csrfToken) headers.set('X-PairRoom-CSRF', state.csrfToken);
    if (options.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
    const response = await fetch(path, { ...options, method, headers, credentials: 'same-origin' });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) {
      const error = new Error(payload.error || response.statusText || `HTTP ${response.status}`);
      error.status = response.status;
      if (response.status === 401 && path !== '/api/v1/session') {
        error.code = 'login_required';
        showCredentialLogin('浏览器会话已过期，请重新输入 Service Token。');
      }
      throw error;
    }
    return payload;
  }

  function snapshotRenderKey(snapshot) {
    if (!snapshot) return '';
    // generated_at is request metadata. A busy Runtime advances last_used_at
    // on every status read, while an idle Runtime's activity timestamp is
    // meaningful to the Runtimes table and must still trigger a refresh.
    const renderableSnapshot = { ...snapshot };
    delete renderableSnapshot.generated_at;
    if (Array.isArray(renderableSnapshot.runtimes)) {
      renderableSnapshot.runtimes = renderableSnapshot.runtimes.map((runtime) => {
        const renderableRuntime = { ...runtime };
        if (renderableRuntime.busy) delete renderableRuntime.last_used_at;
        return renderableRuntime;
      });
    }
    return JSON.stringify(renderableSnapshot);
  }
  async function refresh({ notify = false, forceRender = false } = {}) {
    if (state.refreshPromise) return state.refreshPromise;
    const showProgress = notify || forceRender;
    if (showProgress) $('refresh-button').classList.add('spinning');
    state.refreshPromise = api('/api/v1/service').then((snapshot) => {
      if (!state.authenticated) return null;
      const nextRenderKey = snapshotRenderKey(snapshot);
      const snapshotChanged = nextRenderKey !== state.renderedSnapshotKey;
      state.snapshot = snapshot;
      pruneRoomSelection(snapshot);
      state.connected = true;
      state.lastError = '';
      updateChrome();
      if (forceRender || snapshotChanged) {
        if (forceRender || canRenderNow()) {
          render();
        } else {
          state.renderPending = true;
          window.dispatchEvent(new Event('pairroom:management-render-pending'));
        }
      } else {
        state.renderPending = false;
      }
      if (notify) toast('已同步', 'Management Shell 状态已刷新。', 'success');
      return snapshot;
    }).catch((error) => {
      if (error.status === 401) return null;
      const changed = state.connected || state.lastError !== error.message;
      state.connected = false;
      state.lastError = error.message;
      setDisconnected(error.message);
      if (notify || changed) toast('连接失败', error.message, 'error');
      return null;
    }).finally(() => {
      state.refreshPromise = null;
      if (showProgress) $('refresh-button').classList.remove('spinning');
    });
    return state.refreshPromise;
  }

  function canRenderNow() {
    if (document.querySelector('dialog[open]')) return false;
    const active = document.activeElement;
    if (!active || active === document.body || active === $('global-search')) return true;
    return !view.contains(active) || !['INPUT', 'SELECT', 'TEXTAREA'].includes(active.tagName);
  }

  function scheduleRefresh() {
    if (state.refreshTimer) clearInterval(state.refreshTimer);
    state.refreshTimer = null;
    if (state.preferences.refreshMs > 0) {
      state.refreshTimer = setInterval(() => {
        if (state.authenticated && !document.hidden) refresh();
      }, state.preferences.refreshMs);
    }
  }

  function parseRoute() {
    const raw = location.hash.startsWith('#/') ? location.hash.slice(2) : 'overview';
    const parts = raw.split('/').filter(Boolean).map((part) => {
      try { return decodeURIComponent(part); } catch { return part; }
    });
    if (parts[0] === 'projects' && parts[1]) return { name: 'project', projectID: parts[1] };
    if (parts[0] === 'rooms' && parts[1]) return { name: 'room', roomID: parts[1] };
    if (['overview', 'projects', 'runtimes', 'settings'].includes(parts[0])) return { name: parts[0] };
    return { name: 'overview' };
  }

  function navigate(route) {
    if (location.hash === route) {
      state.route = parseRoute();
      render();
      return;
    }
    location.hash = route;
  }

  function updateChrome() {
    const snapshot = state.snapshot;
    const summary = serviceSummary(snapshot);
    const healthy = Boolean(snapshot?.healthy);
    $('connection-banner').hidden = state.connected;
    $('sidebar-health').textContent = state.connected ? (healthy ? 'Service Healthy' : 'Service Fail-closed') : 'Service Disconnected';
    $('sidebar-version').textContent = snapshot ? `PairRoom ${snapshot.version || ''}`.trim() : 'PairRoom Service';
    $('health-dot').className = `status-dot ${state.connected ? (healthy ? 'good' : 'danger') : 'danger'}`;
    $('nav-project-count').textContent = snapshot ? String(summary.projects) : '';
    $('nav-runtime-count').textContent = snapshot && summary.runtime_capacity_used ? String(summary.runtime_capacity_used) : '';
    const routeInfo = routeMetadata();
    $('page-eyebrow').textContent = routeInfo.eyebrow;
    $('page-title').textContent = routeInfo.title;
    $('page-subtitle').textContent = routeInfo.subtitle;
    document.title = `${routeInfo.title} · PairRoom`;
    document.querySelectorAll('[data-nav]').forEach((node) => {
      const current = state.route.name === 'project' ? 'projects' : (state.route.name === 'room' ? '' : state.route.name);
      node.classList.toggle('active', node.dataset.nav === current);
      if (node.dataset.nav === current) node.setAttribute('aria-current', 'page');
      else node.removeAttribute('aria-current');
    });
    if (snapshot?.generated_at) $('last-updated').textContent = `最近同步 ${formatRelativeTime(snapshot.generated_at)}`;
    syncRoomTree();
    syncRoomTabs();
  }

  function setDisconnected(message) {
    $('connection-banner').hidden = false;
    $('connection-message').textContent = message || '正在等待本地 Service 恢复。';
    $('sidebar-health').textContent = 'Service Disconnected';
    $('health-dot').className = 'status-dot danger';
  }

  function routeMetadata() {
    const snapshot = state.snapshot;
    switch (state.route.name) {
      case 'projects':
        return { eyebrow: 'WORKSPACES', title: 'Projects & Rooms', subtitle: '管理 canonical Git worktree 与彼此隔离的协作 Room。' };
      case 'project': {
        const project = snapshot?.projects?.find((item) => item.id === state.route.projectID);
        return { eyebrow: 'PROJECT', title: projectName(project) || 'Project', subtitle: project?.root || '查看 Project 身份、Room 与运行状态。' };
      }
      case 'runtimes':
        return { eyebrow: 'ORCHESTRATION', title: 'Room Runtimes', subtitle: '查看容量、队列、活动 Turn 与空闲挂起状态。' };
      case 'settings':
        return { eyebrow: 'CONTROL PLANE', title: '设置', subtitle: '调整当前管理页体验，并检查 Service 启动策略与安全边界。' };
      case 'room': {
        const room = roomByID(state.route.roomID);
        const runtime = getRuntime(state.route.roomID);
        return {
          eyebrow: 'ROOM',
          title: room?.name || 'Room',
          subtitle: runtimeLabel(runtime),
        };
      }
      default:
        return { eyebrow: 'PAIRROOM SERVICE', title: '概览', subtitle: 'Claude Code 与 Codex 的多 Project、本地协作控制面。' };
    }
  }

  function render() {
    state.route = parseRoute();
    if (state.route.name === 'room' && state.route.roomID && !state.tabs.includes(state.route.roomID)) {
      state.tabs.push(state.route.roomID);
    }
    updateChrome();
    const roomMode = state.route.name === 'room';
    $('view').hidden = roomMode;
    $('room-stage').hidden = !roomMode;
    $('room-tabstrip').hidden = state.tabs.length === 0;
    if (!state.snapshot) {
      state.renderedSnapshotKey = '';
      state.renderPending = false;
      if (!roomMode) renderLoading();
      return;
    }
    if (roomMode) {
      const room = roomByID(state.route.roomID);
      if (room?.lifecycle === 'archived') {
        toast('Room 已归档', '恢复后才能打开。', 'warning');
        closeTab(state.route.roomID);
        return;
      }
      const runtime = getRuntime(state.route.roomID);
      if (room && !roomHasBlockingPendingBindings(room) && !['active', 'starting', 'queued'].includes(runtime.phase)) {
        activateRoomRuntime(state.route.roomID);
      } else {
        syncRoomStage();
      }
    } else {
      switch (state.route.name) {
        case 'projects': renderProjects(); break;
        case 'project': renderProjectDetail(state.route.projectID); break;
        case 'runtimes': renderRuntimes(); break;
        case 'settings': renderSettings(); break;
        default: renderOverview(); break;
      }
    }
    state.renderedSnapshotKey = snapshotRenderKey(state.snapshot);
    state.renderPending = false;
  }

  function syncRoomTree() {
    const tree = $('room-tree');
    if (!tree) return;
    const snapshot = state.snapshot;
    if (!snapshot) {
      tree.replaceChildren();
      return;
    }
    const models = buildProjectModels(snapshot);
    const activeRoomID = state.route.name === 'room' ? state.route.roomID : '';
    tree.replaceChildren(...models.map((model) => {
      const { project, rooms } = model;
      const expanded = state.expandedProjects.has(project.id) || rooms.some((room) => room.id === activeRoomID) || state.expandedProjects.size === 0;
      if (expanded) state.expandedProjects.add(project.id);
      const activeRooms = rooms.filter((room) => room.lifecycle !== 'archived');
      const archivedRooms = rooms.filter((room) => room.lifecycle === 'archived');
      const showArchived = state.archivedOpen.has(project.id);
      const children = [];
      if (expanded) {
        activeRooms.forEach((room) => children.push(renderTreeRoom(room, room.id === activeRoomID)));
        if (archivedRooms.length) {
          children.push(node('button', {
            type: 'button',
            className: 'tree-archived-toggle',
            textContent: showArchived ? `隐藏已归档 (${archivedRooms.length})` : `已归档 (${archivedRooms.length})`,
            onClick: () => {
              if (showArchived) state.archivedOpen.delete(project.id);
              else state.archivedOpen.add(project.id);
              syncRoomTree();
            },
          }));
          if (showArchived) archivedRooms.forEach((room) => children.push(renderTreeRoom(room, false)));
        }
      }
      return node('section', { className: `tree-project ${expanded ? 'open' : ''}` },
        node('button', {
          type: 'button',
          className: 'tree-project-toggle',
          title: project.root,
          onClick: () => {
            if (state.expandedProjects.has(project.id)) state.expandedProjects.delete(project.id);
            else state.expandedProjects.add(project.id);
            syncRoomTree();
          },
        }, node('span', { className: 'tree-caret', textContent: expanded ? '▾' : '▸' }), node('strong', { textContent: projectName(project) }), node('span', { className: 'nav-count', textContent: String(activeRooms.length) })),
        children.length ? node('div', { className: 'tree-rooms' }, ...children) : null
      );
    }));
  }

  function renderTreeRoom(room, current) {
    const runtime = getRuntime(room.id);
    const archived = room.lifecycle === 'archived';
    const meta = state.tabMeta[room.id] || {};
    const badges = [];
    if (meta.unread) badges.push(node('span', { className: 'tab-badge', textContent: String(meta.unread) }));
    if (meta.pendingApprovals) badges.push(node('span', { className: 'tab-badge warn', textContent: '!' }));
    if (meta.error) badges.push(node('span', { className: 'tab-badge danger', textContent: '×' }));
    return node('button', {
      type: 'button',
      className: `tree-room ${current ? 'active' : ''} ${archived ? 'archived' : ''}`,
      title: archived ? '已归档，恢复后才能打开' : room.name,
      onClick: () => {
        if (archived) {
          toast('Room 已归档', '恢复后才能打开。', 'warning');
          return;
        }
        openRoom(room.id);
      },
    }, node('span', { className: `tree-room-dot ${runtimeTone(runtime)}`, 'aria-hidden': 'true' }), node('span', { className: 'tree-room-name', textContent: room.name }), ...badges);
  }

  function syncRoomTabs() {
    const list = $('room-tablist');
    if (!list) return;
    list.replaceChildren(...state.tabs.map((roomID, index) => {
      const room = roomByID(roomID);
      const runtime = getRuntime(roomID);
      const meta = state.tabMeta[roomID] || {};
      const selected = state.route.name === 'room' && state.route.roomID === roomID;
      const tab = node('div', {
        className: `room-tab ${selected ? 'active' : ''}`,
        role: 'tab',
        draggable: 'true',
        'aria-selected': String(selected),
        'data-room-id': roomID,
        tabindex: selected ? '0' : '-1',
        onClick: () => openRoom(roomID),
        onDragStart: (event) => { state.dragTabID = roomID; event.dataTransfer.setData('text/plain', roomID); },
        onDragOver: (event) => { event.preventDefault(); },
        onDrop: (event) => {
          event.preventDefault();
          reorderTab(state.dragTabID || event.dataTransfer.getData('text/plain'), index);
        },
      }, node('span', { className: `tree-room-dot ${runtimeTone(runtime)}`, 'aria-hidden': 'true' }), node('span', { className: 'room-tab-label', textContent: room?.name || roomID }), meta.unread ? node('span', { className: 'tab-badge', textContent: String(meta.unread) }) : null, node('button', {
        type: 'button',
        className: 'room-tab-close',
        'aria-label': `关闭 ${room?.name || roomID}`,
        textContent: '×',
        onClick: (event) => { event.stopPropagation(); closeTab(roomID); },
      }));
      return tab;
    }));
    syncRoomStage();
  }

  function reorderTab(roomID, toIndex) {
    const from = state.tabs.indexOf(roomID);
    if (from < 0 || from === toIndex) return;
    state.tabs.splice(from, 1);
    state.tabs.splice(toIndex, 0, roomID);
    syncRoomTabs();
  }

  function syncRoomStage() {
    const stage = $('room-stage');
    if (!stage) return;
    const activeID = state.route.name === 'room' ? state.route.roomID : '';
    const keep = new Set(state.tabs);
    Array.from(stage.children).forEach((panel) => {
      if (!keep.has(panel.dataset.roomId)) panel.remove();
    });
    state.tabs.forEach((roomID) => {
      let panel = stage.querySelector(`[data-room-id="${CSS.escape(roomID)}"]`);
      if (!panel) {
        panel = node('div', { className: 'room-panel', 'data-room-id': roomID, role: 'tabpanel' });
        stage.append(panel);
      }
      const selected = roomID === activeID;
      panel.hidden = !selected;
      const runtime = getRuntime(roomID);
      const room = roomByID(roomID);
      const ready = runtime.phase === 'active';
      let frame = panel.querySelector('iframe');
      if (ready) {
        const surface = `/api/v1/rooms/${encodeURIComponent(roomID)}/surface/`;
        if (!frame) {
          frame = node('iframe', { className: 'room-frame', title: room?.name || roomID, src: surface });
          panel.replaceChildren(frame);
        } else if (!frame.src.endsWith('/surface/') && !frame.src.includes(`/surface/`)) {
          frame.src = surface;
        }
      } else {
        if (frame) frame.remove();
        panel.replaceChildren(roomPlaceholder(room, runtime));
      }
      if (frame && selected) notifySurface(frame, 'active');
      else if (frame) notifySurface(frame, 'inactive');
    });
  }

  function roomPlaceholder(room, runtime) {
    const phase = runtime.phase || 'suspended';
    const title = phase === 'queued' ? `排队 #${runtime.queue_position || '?'}` : (phase === 'starting' ? '正在启动 Runtime' : (phase === 'failed' ? 'Runtime 失败' : 'Runtime 已挂起'));
    const detail = runtime.last_error || '切回此标签会自动重新请求激活。后台标签不保证一直在线。';
    return node('div', { className: 'room-placeholder' },
      node('p', { className: 'eyebrow', textContent: 'ROOM SURFACE' }),
      node('h2', { textContent: room?.name || 'Room' }),
      node('p', { textContent: `${title} · ${detail}` }),
      actionButton('重新激活', () => activateRoomRuntime(room.id), 'primary-button')
    );
  }

  function notifySurface(frame, action) {
    try {
      const target = frame.contentWindow;
      if (!target) return;
      target.postMessage({ type: 'pairroom-shell', action, roomId: frame.parentElement?.dataset.roomId || '' }, window.location.origin);
    } catch {
      // Cross-document messaging is best-effort until the iframe finishes loading.
    }
  }

  function closeTab(roomID) {
    const index = state.tabs.indexOf(roomID);
    if (index < 0) return;
    state.tabs.splice(index, 1);
    delete state.tabMeta[roomID];
    const stage = $('room-stage');
    stage?.querySelector(`[data-room-id="${CSS.escape(roomID)}"]`)?.remove();
    if (state.route.name === 'room' && state.route.roomID === roomID) {
      const next = state.tabs[index] || state.tabs[index - 1];
      if (next) navigate(`#/rooms/${encodeURIComponent(next)}`);
      else navigate('#/overview');
      return;
    }
    syncRoomTabs();
  }

  async function activateRoomRuntime(roomID) {
    if (!roomID || state.activating.has(roomID)) return null;
    const runtime = getRuntime(roomID);
    if (['active', 'starting', 'queued'].includes(runtime.phase)) {
      syncRoomStage();
      return runtime;
    }
    state.activating.add(roomID);
    try {
      const status = await api(`/api/v1/rooms/${encodeURIComponent(roomID)}/activate`, { method: 'POST' });
      await refresh({ forceRender: true });
      return status;
    } catch (error) {
      toast('Room 激活失败', error.message, 'error');
      return null;
    } finally {
      state.activating.delete(roomID);
    }
  }

  function shiftTab(delta) {
    if (!state.tabs.length) return;
    const current = state.route.name === 'room' ? state.tabs.indexOf(state.route.roomID) : 0;
    const next = (Math.max(current, 0) + delta + state.tabs.length) % state.tabs.length;
    openRoom(state.tabs[next]);
  }

  function moveActiveTab(delta) {
    if (state.route.name !== 'room') return;
    const from = state.tabs.indexOf(state.route.roomID);
    if (from < 0) return;
    const to = Math.max(0, Math.min(state.tabs.length - 1, from + delta));
    reorderTab(state.route.roomID, to);
  }

  function openRoomPicker() {
    renderRoomPicker();
    showDialog('room-picker-dialog');
    queueMicrotask(() => $('room-picker-search').focus());
  }

  function renderRoomPicker() {
    const query = ($('room-picker-search')?.value || '').trim().toLocaleLowerCase();
    const rooms = (state.snapshot?.rooms || []).filter((room) => room.lifecycle !== 'archived');
    const matches = rooms.filter((room) => {
      if (!query) return true;
      const project = projectForRoom(state.snapshot, room);
      return [room.name, room.id, projectName(project)].join('\n').toLocaleLowerCase().includes(query);
    });
    const list = $('room-picker-list');
    list.replaceChildren(...(matches.length ? matches.map((room) => {
      const project = projectForRoom(state.snapshot, room);
      return node('button', {
        type: 'button',
        className: 'list-item room-picker-item',
        onClick: () => { closeDialog('room-picker-dialog'); openRoom(room.id); },
      }, node('div', { className: 'list-copy' }, node('strong', { textContent: room.name }), node('p', { textContent: projectName(project) })));
    }) : [node('p', { className: 'muted', textContent: '没有匹配的活动 Room。' })]));
  }

  function renderLoading() {
    view.replaceChildren(
      node('div', { className: 'view-stack' },
        node('div', { className: 'loading-grid' },
          ...Array.from({ length: 4 }, () => node('div', { className: 'panel skeleton loading-card' }))
        ),
        node('div', { className: 'panel skeleton', style: 'height: 300px' })
      )
    );
  }

  function renderOverview() {
    const snapshot = state.snapshot;
    const summary = serviceSummary(snapshot);
    const projects = snapshot.projects || [];
    const activeProjects = projects.filter((project) => project.available);
    const policy = runtimePolicy(snapshot);
    const attention = attentionItems(snapshot);
    const live = liveRuntimeItems(snapshot);
    const heroTitle = projects.length
      ? `${summary.active_rooms} 个活动 Room，跨 ${summary.projects} 个 Project 协作`
      : '从第一个 Git Project 开始建立协作控制面';
    const heroText = projects.length
      ? '每个 Room 独占 Claude Session 与 Codex Thread；切换管理视图不会中断后台 Turn。'
      : '显式登记 canonical Git worktree，再为不同任务创建彼此隔离的 Claude × Codex Room。';

    const heroActions = [
      actionButton('＋ 登记 Project', () => openProjectDialog('register'), 'primary-button'),
    ];
    if (activeProjects.length) heroActions.push(actionButton('＋ 创建 Room', () => openRoomDialog(activeProjects[0].id), 'secondary-button'));

    view.replaceChildren(
      node('div', { className: 'view-stack' },
        node('section', { className: 'panel hero-panel' },
          node('div', { className: 'hero-copy' },
            node('p', { className: 'eyebrow', textContent: 'LOCAL PAIRING CONTROL PLANE' }),
            node('h2', { textContent: heroTitle }),
            node('p', { textContent: heroText }),
            node('div', { className: 'hero-actions' }, ...heroActions)
          ),
          node('div', { className: 'hero-meta' },
            heroMeta('Runtime 容量', policy.limit ? `${summary.runtime_capacity_used} / ${policy.limit}` : `${summary.runtime_capacity_used}`),
            heroMeta('空闲挂起', policy.idle_timeout_seconds ? formatDuration(policy.idle_timeout_seconds) : '由启动参数决定'),
            heroMeta('运行模式', snapshot.healthy ? 'Fail-safe' : 'Fail-closed'),
            heroMeta('版本', snapshot.version || 'development')
          )
        ),
        node('section', { className: 'stats-grid', 'aria-label': 'Service 统计' },
          statCard('Projects', summary.projects, `${summary.unavailable_projects} 个不可用`, '⌂', 'accent', () => navigate('#/projects')),
          statCard('活动 Room', summary.active_rooms, `${summary.archived_rooms} 个已归档`, '◇', 'good', () => navigate('#/projects')),
          statCard('正在工作', summary.busy_runtimes, `${summary.queued_runtimes} 个排队`, '◎', summary.queued_runtimes ? 'warn' : '', () => navigate('#/runtimes')),
          statCard('需要关注', summary.attention_items, summary.attention_items ? 'Binding、Runtime、路径或清理诊断' : '当前无阻断项', '!', summary.attention_items ? 'danger' : 'good')
        ),
        node('section', { className: 'two-panel-grid' },
          panel('需要关注', '阻断激活或需要人工处理的项目',
            attention.length ? node('div', { className: 'list' }, ...attention.slice(0, 8).map(renderAttentionItem))
              : emptyState('✓', '一切正常', '当前没有不可用 Project、失败 Runtime、待补全 Binding 或 Room 清理项。', true),
            attention.length > 8 ? `${attention.length - 8} 个项目未显示` : ''
          ),
          panel('实时运行', '活跃、工作中与排队的 Room',
            live.length ? node('div', { className: 'list' }, ...live.slice(0, 8).map(renderLiveItem))
              : emptyState('◎', '当前没有活动 Runtime', '打开一个 Room 后，Runtime 会按容量惰性启动。', true)
          )
        ),
        panel('Projects', '最近登记的工作区与 Room 概况',
          projects.length ? node('div', { className: 'list' }, ...projects.slice(0, 6).map(renderProjectOverviewItem))
            : emptyState('⌂', '尚未登记 Project', '只接受用户显式输入的绝对路径，不扫描开发目录。', true, actionButton('登记第一个 Project', () => openProjectDialog('register'), 'primary-button compact-button')),
          projects.length > 6 ? '在 Projects 页面查看全部工作区' : '',
          projects.length > 6 ? actionButton('查看全部', () => navigate('#/projects'), 'text-button') : null
        ),
        node('aside', { className: 'callout boundary' },
          node('strong', { textContent: 'Transcript Boundary' }),
          node('span', { textContent: '复用 Existing Session/Thread 只恢复 vendor context；PairRoom 的公共时间线从绑定成功后开始，不导入绑定前 transcript。' })
        )
      )
    );
  }

  function renderProjects() {
    const snapshot = state.snapshot;
    const projectModels = buildProjectModels(snapshot)
      .filter((model) => projectMatchesFilters(model))
      .sort((a, b) => Number(b.project.available) - Number(a.project.available) || Number(b.runtimeCounts.busy) - Number(a.runtimeCounts.busy) || projectName(a.project).localeCompare(projectName(b.project)));

    const availability = selectControl([
      ['all', '全部状态'], ['available', 'Available'], ['unavailable', 'Unavailable'],
    ], state.filters.projectAvailability, (value) => { state.filters.projectAvailability = value; renderProjects(); }, 'Project 状态');

    view.replaceChildren(
      node('div', { className: 'view-stack' },
        node('section', { className: 'section-header' },
          node('div', {}, node('h2', { textContent: 'Workspace 管理' }), node('p', { textContent: '一个 Project 对应一个 canonical Git worktree；每个 Project 可以拥有多个 Room。' })),
          node('div', { className: 'section-actions' },
            actionButton('导入 Legacy Room', () => openProjectDialog('import'), 'secondary-button'),
            actionButton('＋ 登记 Project', () => openProjectDialog('register'), 'primary-button')
          )
        ),
        node('section', { className: 'panel panel-body toolbar' },
          node('label', { className: 'search-field' },
            node('span', { textContent: '⌕', 'aria-hidden': 'true' }),
            node('input', {
              type: 'search', value: state.search, placeholder: '筛选路径、Project、Room 或 ID',
              'aria-label': '筛选 Project 和 Room',
              onInput: (event) => { state.search = event.target.value; $('global-search').value = state.search; renderProjects(); },
            })
          ),
          availability,
          actionButton(state.filters.showArchived ? '✓ 显示已归档' : '显示已归档', () => {
            state.filters.showArchived = !state.filters.showArchived;
            renderProjects();
          }, `filter-chip ${state.filters.showArchived ? 'active' : ''}`),
          state.snapshot?.capabilities?.room_deletion
            ? roomSelectionToggleButton(visibleRoomsForBatch(projectModels.flatMap((model) => model.rooms)), '当前可见结果')
            : null,
          state.snapshot?.capabilities?.room_deletion
            ? roomClearSelectionButton()
            : null,
          state.snapshot?.capabilities?.room_deletion
            ? roomBatchArchiveButton(selectedActiveRooms(visibleRoomsForBatch(projectModels.flatMap((model) => model.rooms))))
            : null,
          state.snapshot?.capabilities?.room_deletion
            ? roomBatchRemovalButton(selectedArchivedRooms(visibleRoomsForBatch(projectModels.flatMap((model) => model.rooms))))
            : null,
          node('span', { className: 'muted', textContent: `${projectModels.length} / ${(snapshot.projects || []).length} Projects` })
        ),
        projectModels.length
          ? node('section', { className: 'project-grid' }, ...projectModels.map(renderProjectCard))
          : emptyState('⌕', '没有匹配的 Project', state.search ? '调整搜索词或筛选条件。' : '登记一个 Git worktree 开始使用。', false,
            state.search ? actionButton('清除筛选', () => { state.search = ''; $('global-search').value = ''; state.filters.projectAvailability = 'all'; renderProjects(); }, 'secondary-button')
              : actionButton('登记 Project', () => openProjectDialog('register'), 'primary-button')),
        node('aside', { className: 'callout neutral' },
          node('strong', { textContent: 'Project Identity' }),
          node('span', { textContent: '服务会解析符号链接并执行 Git worktree 根目录归一化；等价路径不会创建重复 Project。' })
        )
      )
    );
  }

  function renderProjectCard(model) {
    const { project, rooms, activeRooms, runtimeCounts } = model;
    const visibleRooms = rooms.filter((room) => state.filters.showArchived || room.lifecycle !== 'archived');
    const archivedRoomsHidden = !state.filters.showArchived && rooms.length > 0 && visibleRooms.length === 0;
    const emptyRoomTitle = archivedRoomsHidden ? '此 Project 只剩已归档 Room' : '此 Project 尚无 Room';
    const emptyRoomDescription = archivedRoomsHidden
      ? '显示并永久清除已归档 Room 后即可注销 Project。'
      : project.available
        ? '创建一个 Room，绑定 Claude 与 Codex。'
        : '本地路径不可用不影响注销此空 Project。';
    const emptyRoomAction = archivedRoomsHidden
      ? actionButton('显示已归档 Room', () => {
          state.filters.showArchived = true;
          renderProjects();
        }, 'secondary-button compact-button')
      : project.available
        ? actionButton('创建 Room', () => openRoomDialog(project.id), 'secondary-button compact-button')
        : null;
    return node('article', { className: 'panel project-card' },
      node('header', { className: 'project-card-header' },
        node('div', { className: 'project-avatar', textContent: projectInitials(project) }),
        node('div', { className: 'project-card-title' },
          node('div', {}, node('h2', { textContent: projectName(project) }), statusBadge(project.available ? 'available' : 'unavailable', project.available ? 'good' : 'danger')),
          node('code', { className: 'project-path', textContent: project.root, title: project.root })
        ),
        node('div', { className: 'project-card-actions' },
          state.snapshot?.capabilities?.project_refresh && !project.available
            ? actionButton('重新检查', () => refreshProject(project), 'secondary-button compact-button')
            : null,
          actionButton('复制路径', () => copyText(project.root, 'Project 路径已复制。'), 'secondary-button compact-button'),
          actionButton('详情', () => navigate(`#/projects/${encodeURIComponent(project.id)}`), 'secondary-button compact-button'),
          projectRemovalButton(project, rooms.length, true),
          actionButton('＋ Room', () => openRoomDialog(project.id), 'primary-button compact-button', !project.available)
        )
      ),
      node('div', { className: 'project-summary' },
        summaryCell(activeRooms, '活动 Room'),
        summaryCell(rooms.length - activeRooms, '已归档'),
        summaryCell(runtimeCounts.busy, '工作中'),
        summaryCell(runtimeCounts.queued, '排队')
      ),
      project.diagnostic ? node('div', { className: 'callout danger', style: 'margin: 12px 14px 0' }, node('strong', { textContent: '路径诊断' }), node('span', { textContent: project.diagnostic })) : null,
      visibleRooms.length
        ? node('div', { className: 'room-list' }, ...visibleRooms.map((room) => renderRoomRow(room, model.runtimeByRoom.get(room.id))))
        : emptyState('◇', emptyRoomTitle, emptyRoomDescription, true, emptyRoomAction)
    );
  }

  function renderProjectDetail(projectID) {
    const snapshot = state.snapshot;
    const model = buildProjectModels(snapshot).find((item) => item.project.id === projectID);
    if (!model) {
      view.replaceChildren(emptyState('?', 'Project 不存在', '它可能尚未登记，或 Service Registry 已重新构建。', false, actionButton('返回 Projects', () => navigate('#/projects'), 'secondary-button')));
      return;
    }
    const { project, rooms, activeRooms, runtimeCounts, runtimeByRoom } = model;
    const visibleRooms = rooms.filter((room) => state.filters.showArchived || room.lifecycle !== 'archived');
    view.replaceChildren(
      node('div', { className: 'view-stack' },
        actionButton('← 返回 Projects', () => navigate('#/projects'), 'text-button'),
        node('section', { className: 'panel detail-hero' },
          node('div', { className: 'detail-title' },
            node('div', { className: 'project-avatar', textContent: projectInitials(project) }),
            node('div', {},
              node('div', { className: 'room-title-line' }, node('h2', { textContent: projectName(project), style: 'margin:0' }), statusBadge(project.available ? 'available' : 'unavailable', project.available ? 'good' : 'danger')),
              node('p', { className: 'detail-path', textContent: project.root })
            )
          ),
          node('div', { className: 'detail-actions' },
            actionButton('复制路径', () => copyText(project.root, 'Project 路径已复制。'), 'secondary-button'),
            actionButton('＋ 创建 Room', () => openRoomDialog(project.id), 'primary-button', !project.available)
          )
        ),
        node('section', { className: 'stats-grid' },
          statCard('活动 Room', activeRooms, `${rooms.length - activeRooms} 已归档`, '◇', 'accent'),
          statCard('工作中', runtimeCounts.busy, `${runtimeCounts.active} active`, '◎', runtimeCounts.busy ? 'warn' : ''),
          statCard('排队', runtimeCounts.queued, runtimeCounts.queued ? '等待全局容量' : '当前无等待', '↥', runtimeCounts.queued ? 'warn' : 'good'),
          statCard('失败', runtimeCounts.failed, runtimeCounts.failed ? '查看 Runtime 诊断' : '无失败 Runtime', '!', runtimeCounts.failed ? 'danger' : 'good')
        ),
        node('section', { className: 'two-panel-grid' },
          panel('Rooms', '公共时间线、附件、审批与 Agent Bindings 均按 Room 隔离',
            visibleRooms.length ? node('div', { className: 'room-list' }, ...visibleRooms.map((room) => renderRoomRow(room, runtimeByRoom.get(room.id))))
              : emptyState('◇', '没有可见 Room', '创建新 Room，或显示已归档 Room。', true),
            '',
            node('div', { className: 'section-actions' },
              actionButton(state.filters.showArchived ? '隐藏已归档' : '显示已归档', () => { state.filters.showArchived = !state.filters.showArchived; renderProjectDetail(projectID); }, 'secondary-button compact-button'),
              state.snapshot?.capabilities?.room_deletion ? roomSelectionToggleButton(visibleRooms, '本项目当前可见范围') : null,
              state.snapshot?.capabilities?.room_deletion ? roomClearSelectionButton() : null,
              state.snapshot?.capabilities?.room_deletion ? roomBatchArchiveButton(selectedActiveRooms(visibleRooms)) : null,
              state.snapshot?.capabilities?.room_deletion ? roomBatchRemovalButton(selectedArchivedRooms(visibleRooms)) : null,
              actionButton('＋ Room', () => openRoomDialog(project.id), 'primary-button compact-button', !project.available)
            )
          ),
          panel('Project Identity', 'Registry 中的 canonical worktree 记录',
            node('div', { className: 'key-value-grid' },
              keyValue('Project ID', project.id, true),
              keyValue('创建时间', formatDateTime(project.created_at)),
              keyValue('Canonical Root', project.root, true),
              keyValue('可用性', project.available ? 'Available' : 'Unavailable')
            ),
            project.diagnostic || 'Service 不会从当前工作目录隐式切换 Project。'
          )
        ),
        state.snapshot?.capabilities?.project_refresh || state.snapshot?.capabilities?.project_removal
          ? panel('Project Maintenance', '重新检查 canonical path，或安全注销空 Project。',
            node('div', { className: 'section-actions' },
              state.snapshot?.capabilities?.project_refresh
                ? actionButton('重新检查路径', () => refreshProject(project), 'secondary-button')
                : null,
              projectRemovalButton(project, rooms.length)
            ),
            rooms.length
              ? `仍包含 ${rooms.length} 个 Room（含已归档）；请先归档并永久清除全部 Room，再注销 Project。`
              : '注销只移除 Registry 登记，不删除 Git worktree 或 vendor context。'
          )
          : null,
        project.diagnostic ? node('aside', { className: 'callout danger' }, node('strong', { textContent: 'Project 不可用' }), node('span', { textContent: project.diagnostic })) : null,
        node('aside', { className: 'callout boundary' }, node('strong', { textContent: 'Workspace Boundary' }), node('span', { textContent: 'Room 永久属于此 Project；Reviewer snapshot 与 Git 状态都以该 canonical worktree 为边界。' }))
      )
    );
  }

  function renderRoomRow(room, runtime = suspendedRuntime(room.id)) {
    const pending = roomHasBlockingPendingBindings(room);
    const archived = room.lifecycle === 'archived';
    const title = node('div', { className: 'room-title-line' },
      node('strong', { textContent: room.name }),
      statusBadge(room.lifecycle || 'active', archived ? 'warn' : 'good'),
      statusBadge(runtimeLabel(runtime), runtimeTone(runtime), runtime.busy ? 'busy' : ''),
      room.legacy ? statusBadge('legacy', 'info') : null
    );
    const meta = node('div', { className: 'room-meta' },
      bindingMeta('claude', room.bindings?.claude),
      bindingMeta('codex', room.bindings?.codex),
      node('code', { textContent: room.id, title: room.id })
    );
    if (runtime.last_error) meta.append(node('span', { className: 'badge danger plain', textContent: truncate(runtime.last_error, 90), title: runtime.last_error }));

    const actions = node('div', { className: 'room-actions' });
    if (state.snapshot?.capabilities?.room_deletion) {
      actions.append(
        node('label', {
          className: 'button secondary-button compact-button room-action-control room-select-control',
          title: archived ? '选择此 Room 进行批量清理' : '选择此 Room 进行批量归档',
        },
          node('input', {
            type: 'checkbox',
            checked: state.selectedRoomIDs.has(room.id),
            'aria-label': `选择 Room ${room.name}`,
            onChange: (event) => toggleRoomSelection(room.id, event.target.checked),
          }),
          node('span', { textContent: '选择' })
        )
      );
    }
    if (archived) {
      actions.append(actionButton('恢复', () => restoreRoom(room), 'secondary-button compact-button room-action-control'));
      if (state.snapshot?.capabilities?.room_deletion) {
        actions.append(actionButton('永久清除', () => confirmRoomRemoval([room]), 'danger-button outline compact-button room-action-control'));
      }
    } else {
      if (pending) actions.append(actionButton('补全 Binding', () => completeBindings(room), 'primary-button compact-button room-action-control'));
      actions.append(actionButton(runtime.phase === 'queued' ? `排队 #${runtime.queue_position || '?'}` : '打开', () => openRoom(room.id), 'primary-button compact-button room-action-control', pending));
      actions.append(actionButton('浏览器打开', () => openRoomInBrowserAction(room.id), 'secondary-button compact-button room-action-control', pending));
      actions.append(actionButton('重命名', () => openRenameDialog(room), 'secondary-button compact-button room-action-control'));
      actions.append(actionButton('归档', () => archiveRoom(room), 'danger-button outline compact-button room-action-control'));
    }
    return node('article', { className: 'room-row' }, node('div', { className: 'room-row-main' }, title, meta), actions);
  }

  function renderRuntimes() {
    const snapshot = state.snapshot;
    const policy = runtimePolicy(snapshot);
    const summary = serviceSummary(snapshot);
    const models = runtimeModels(snapshot)
      .filter((item) => state.filters.runtimePhase === 'all' || item.runtime.phase === state.filters.runtimePhase)
      .sort(compareRuntimeModels);
    const queued = runtimeModels(snapshot).filter((item) => item.runtime.phase === 'queued').sort((a, b) => (a.runtime.queue_position || 9999) - (b.runtime.queue_position || 9999));
    const limit = policy.limit || Math.max(summary.runtime_capacity_used, 1);
    const percent = Math.min(100, Math.round((summary.runtime_capacity_used / limit) * 100));
    const phaseSelect = selectControl([
      ['all', '全部 Runtime'], ['active', 'Active'], ['queued', 'Queued'], ['starting', 'Starting'], ['stopping', 'Stopping'], ['suspended', 'Suspended'], ['failed', 'Failed'],
    ], state.filters.runtimePhase, (value) => { state.filters.runtimePhase = value; renderRuntimes(); }, 'Runtime 阶段');

    view.replaceChildren(
      node('div', { className: 'view-stack' },
        node('section', { className: 'section-header' },
          node('div', {}, node('h2', { textContent: 'Runtime Orchestration' }), node('p', { textContent: '只回收 idle Runtime；活动 Turn 永不因容量、切换页面或 Service 设置而被抢占。' })),
          node('div', { className: 'section-actions' }, phaseSelect, actionButton('刷新状态', () => refresh({ notify: true, forceRender: true }), 'secondary-button'))
        ),
        node('section', { className: 'three-panel-grid' },
          node('article', { className: 'panel capacity-card' },
            node('div', { className: 'capacity-heading' }, node('div', {}, node('div', { className: 'stat-label', textContent: '全局 Runtime 容量' }), node('div', { className: 'capacity-value' }, String(summary.runtime_capacity_used), node('small', { textContent: ` / ${policy.limit || '—'}` }))), statusBadge(percent >= 100 ? 'full' : 'available', percent >= 100 ? 'warn' : 'good')),
            node('div', { className: 'progress-track', role: 'progressbar', 'aria-valuemin': '0', 'aria-valuemax': String(limit), 'aria-valuenow': String(summary.runtime_capacity_used) }, node('div', { className: 'progress-bar', style: `width:${percent}%` })),
            node('div', { className: 'capacity-legend' }, node('span', {}, node('i'), `${summary.active_runtimes} active`), node('span', {}, node('i', { className: 'busy' }), `${summary.busy_runtimes} working`), node('span', {}, node('i', { className: 'queued' }), `${summary.queued_runtimes} queued`))
          ),
          statCard('Idle timeout', policy.idle_timeout_seconds ? formatDuration(policy.idle_timeout_seconds) : '—', '从最后活动开始计算', '◷', 'accent'),
          statCard('Queue', summary.queued_runtimes, summary.queued_runtimes ? 'FIFO，断开浏览器不会取消需求' : '当前无等待', '↥', summary.queued_runtimes ? 'warn' : 'good')
        ),
        queued.length ? panel('Activation Queue', '所有容量都被工作中 Runtime 占用时，新 Room 进入可见 FIFO 队列',
          node('div', { className: 'list' }, ...queued.map((item) => renderQueueItem(item)))
        ) : null,
        panel('所有 Room Runtime', `${models.length} 个 Room · 状态按工作优先级排序`,
          models.length ? runtimeTable(models) : emptyState('◎', '没有匹配的 Runtime', '调整状态筛选。', true),
          'Failed 且仍占用容量表示清理状态不确定；Service 会 fail closed，避免同一 Binding 启动第二个 Runtime。'
        ),
        node('aside', { className: 'callout warning' },
          node('strong', { textContent: 'Non-preemptive policy' }),
          node('span', { textContent: '“挂起”只对 idle、queued 或可安全关闭的 Runtime 生效。Busy Runtime 会返回冲突，必须等待 Turn 完成或在 Room View 中由用户显式中断。' })
        )
      )
    );
  }

  function runtimeTable(models) {
    const table = node('table', { className: 'runtime-table' });
    table.append(node('thead', {}, node('tr', {}, ...['Room', 'Project', '阶段', '容量', '最后活动', '操作'].map((label) => node('th', { textContent: label })))));
    const body = node('tbody');
    models.forEach(({ room, project, runtime }) => {
      const actionCell = node('div', { className: 'runtime-actions' });
      const cleanupUncertain = runtime.phase === 'failed' && runtime.occupies_capacity;
      if (room.lifecycle !== 'archived') {
        actionCell.append(actionButton(
          cleanupUncertain ? '需受控重启' : (runtime.phase === 'active' ? '打开' : '激活'),
          () => openRoom(room.id),
          'secondary-button compact-button',
          roomHasBlockingPendingBindings(room) || cleanupUncertain,
        ));
      }
      if (['active', 'queued', 'starting'].includes(runtime.phase)) {
        actionCell.append(actionButton(runtime.phase === 'queued' ? '取消排队' : '挂起', () => suspendRoom(room, runtime), 'danger-button outline compact-button', runtime.busy || runtime.phase === 'starting'));
      }
      body.append(node('tr', { className: 'runtime-record' },
        node('td', { 'data-label': 'Room' }, node('div', { className: 'runtime-room' }, node('strong', { textContent: room.name }), node('small', { textContent: room.id }))),
        node('td', { 'data-label': 'Project', textContent: projectName(project) || 'Unknown' }),
        node('td', { 'data-label': '阶段' }, statusBadge(runtimeLabel(runtime), runtimeTone(runtime), runtime.busy ? 'busy' : '')),
        node('td', { 'data-label': '容量', textContent: runtime.occupies_capacity ? '占用' : '—' }),
        node('td', { 'data-label': '最后活动', textContent: runtime.last_used_at ? formatRelativeTime(runtime.last_used_at) : '—', title: runtime.last_used_at ? formatDateTime(runtime.last_used_at) : '' }),
        node('td', { 'data-label': '操作' }, actionCell)
      ));
      if (runtime.last_error) {
        body.append(node('tr', { className: 'runtime-error-row' }, node('td', { colspan: '6', className: 'runtime-error-cell' }, node('div', { className: 'callout danger' }, node('strong', { textContent: 'Runtime error' }), node('span', { textContent: runtime.last_error })))));
      }
    });
    table.append(body);
    return node('div', { className: 'panel-body flush' }, table);
  }

  function renderQueueItem({ room, project, runtime }) {
    return node('article', { className: 'list-item' },
      node('div', { className: 'list-main' }, node('div', { className: 'item-symbol accent', textContent: `#${runtime.queue_position || '?'}` }), node('div', { className: 'list-copy' }, node('strong', { textContent: room.name }), node('p', { textContent: `${projectName(project)} · queued ${runtime.queued_at ? formatRelativeTime(runtime.queued_at) : ''}`.trim() }))),
      node('div', { className: 'list-meta' }, actionButton('取消排队', () => suspendRoom(room, runtime), 'secondary-button compact-button'))
    );
  }

  function renderSettings() {
    const sections = [
      ['interface', '界面体验'], ['runtime', 'Runtime 策略'], ['operations', 'Daemon 运维'], ['service', 'Service 与诊断'], ['boundaries', '安全边界'],
    ];
    const nav = node('nav', { className: 'panel settings-nav', 'aria-label': '设置分区' }, ...sections.map(([key, label]) => {
      const active = state.settingsSection === key;
      const button = actionButton(label, () => { state.settingsSection = key; renderSettings(); }, active ? 'active' : '');
      button.setAttribute('aria-pressed', String(active));
      return button;
    }));
    const content = node('section', { className: 'settings-content' }, renderSettingsSection());
    view.replaceChildren(
      node('div', { className: 'view-stack' },
        node('section', { className: 'section-header' },
          node('div', {}, node('h2', { textContent: 'Management Settings' }), node('p', { textContent: '界面偏好只作用于当前标签页；Service 策略由显式启动参数决定。' })),
          actionButton('导出脱敏诊断', downloadDiagnosticSnapshot, 'secondary-button')
        ),
        node('div', { className: 'settings-grid' }, nav, content)
      )
    );
  }

  function renderSettingsSection() {
    const snapshot = state.snapshot;
    const policy = runtimePolicy(snapshot);
    if (state.settingsSection === 'runtime') {
      const command = runtimeCommand(policy);
      return node('div', { className: 'view-stack' },
        settingsPanel('有效 Runtime 策略', '当前进程实际采用的不可抢占调度参数。',
          settingRow('最大活动 Runtime', 'Starting、Active、Stopping，以及清理不确定的 Failed Runtime 都占用容量。降低上限不会打断正在跑的 Turn。', runtimeLimitControl(policy)),
          settingRow('Idle timeout', '只在 Runtime 没有活动 Turn 时开始计算。', node('strong', { textContent: policy.idle_timeout_seconds ? formatDuration(policy.idle_timeout_seconds) : '未暴露' })),
          settingRow('Reconcile interval', 'Runtime Manager 检查空闲、队列和容量的频率。', node('strong', { textContent: policy.poll_interval_milliseconds ? `${policy.poll_interval_milliseconds} ms` : '未暴露' })),
          settingRow('Close timeout', '安全关闭 Room Runtime 的单次截止时间。', node('strong', { textContent: policy.close_timeout_seconds ? formatDuration(policy.close_timeout_seconds) : '未暴露' }))
        ),
        node('section', { className: 'panel' },
          node('header', { className: 'panel-header' }, node('div', { className: 'panel-header-copy' }, node('h2', { textContent: '调整启动参数' }), node('p', { textContent: '策略变更需要受控重启，避免运行中的 vendor session 被动态重配。' }))),
          node('div', { className: 'panel-body' },
            node('div', { className: 'command-box' }, node('code', { textContent: command }), actionButton('复制', () => copyText(command, '启动命令已复制。'), 'secondary-button compact-button'))
          ),
          node('footer', { className: 'panel-footer', textContent: '该前台示例只覆盖 Runtime 参数；请保留当前进程使用的 --config、--data-root、listen、token 等其他启动参数。' })
        ),
        node('aside', { className: 'callout warning' }, node('strong', { textContent: '容量与 idle' }), node('span', { textContent: '容量可在此页立即调整；降低上限不会打断正在跑的 Turn。Idle timeout 仍由启动参数决定，后台标签不保证一直占有 Runtime。' }))
      );
    }
    if (state.settingsSection === 'operations') {
      const daemonInstall = daemonInstallCommand(policy);
      return node('div', { className: 'view-stack' },
        settingsPanel('Daemon 快捷命令', 'PairRoom Web Shell 不直接停止或重启承载自身的宿主进程；运维动作在本机终端执行。',
          settingRow('打开 Management Shell', '解析并验证当前 daemon 的完整认证地址后交给默认浏览器。', inlineCommand('pairroom daemon open', 'Daemon open 命令已复制。')),
          settingRow('检查状态', '显示安装状态、平台、PID、日志与轮转元数据。', inlineCommand('pairroom daemon status', 'Daemon status 命令已复制。')),
          settingRow('跟随日志', '读取 daemon 管理的合并 stdout/stderr 日志。', inlineCommand('pairroom daemon logs -f', 'Daemon logs 命令已复制。')),
          settingRow('受控重启', '沿用已安装的完整 Service 定义，并等待活动 Turn 排空。', inlineCommand('pairroom daemon restart', 'Daemon restart 命令已复制。'))
        ),
        node('section', { className: 'panel' },
          node('header', { className: 'panel-header' }, node('div', { className: 'panel-header-copy' }, node('h2', { textContent: '更新已安装的 Runtime 参数' }), node('p', { textContent: 'daemon restart 不接受新的 Service 参数；需要重新写入完整安装定义。' }))),
          node('div', { className: 'panel-body command-stack' },
            node('div', { className: 'command-example' },
              node('div', { className: 'command-example-heading' }, node('strong', { textContent: '默认参数示例' }), node('span', { textContent: '复制前核对现有 daemon 定义' })),
              node('div', { className: 'command-box' }, node('code', { textContent: daemonInstall }), actionButton('复制示例', () => copyText(daemonInstall, 'Daemon install 示例已复制。'), 'secondary-button compact-button'))
            )
          ),
          node('footer', { className: 'panel-footer', textContent: '--force 会替换服务定义。若当前使用自定义 --config、--data-root、listen、token、proxy 或日志参数，必须在命令中完整保留。' })
        ),
        node('aside', { className: 'callout boundary' }, node('strong', { textContent: 'Daemon boundary' }), node('span', { textContent: 'daemon 只把 pairroom service 投射到 systemd、launchd 或 Windows Task Scheduler；Room Runtime、审批与 Agent Harness 生命周期仍由 PairRoom Service 管理。' })),
        node('aside', { className: 'callout warning' }, node('strong', { textContent: 'Crash-stale service.lock' }), node('span', { textContent: '只有确认旧进程已经消失后，才可显式执行 pairroom daemon start 或 restart --recover-stale-lock；正常启动不会自动猜测锁已失效。' }))
      );
    }
    if (state.settingsSection === 'service') {
      const safe = diagnosticSnapshot();
      return node('div', { className: 'view-stack' },
        node('section', { className: 'panel' },
          node('header', { className: 'panel-header' }, node('div', { className: 'panel-header-copy' }, node('h2', { textContent: 'Service Identity' }), node('p', { textContent: '构建信息与稳定数据根。' }))),
          node('div', { className: 'key-value-grid' },
            keyValue('Version', snapshot.version || 'development'),
            keyValue('Commit', snapshot.commit || 'not embedded', true),
            keyValue('Build date', snapshot.build_date || 'not embedded'),
            keyValue('Generated at', formatDateTime(snapshot.generated_at)),
            keyValue('Data root', snapshot.data_root, true),
            keyValue('Registry health', snapshot.healthy ? 'Healthy' : 'Fail-closed')
          )
        ),
        settingsPanel('诊断工具', '导出内容会移除 Room Runtime URL；本机路径和业务元数据仍可能敏感。',
          settingRow('复制 Service 摘要', '适合粘贴到本地 Issue 或调试会话。', actionButton('复制 JSON', () => copyText(JSON.stringify(safe, null, 2), '脱敏诊断已复制。'), 'secondary-button')),
          settingRow('下载诊断文件', '文件只在浏览器本地生成，不上传到外部服务。', actionButton('下载 JSON', downloadDiagnosticSnapshot, 'secondary-button')),
          settingRow('查看原始结构', '在页面中展开脱敏后的 Service snapshot。', toggleButton(state.showRawSnapshot, (value) => { state.showRawSnapshot = value; renderSettings(); }, '切换 snapshot 显示'))
        ),
        state.showRawSnapshot ? node('pre', { className: 'raw-snapshot', textContent: JSON.stringify(safe, null, 2) }) : null,
        snapshot.maintenance?.pending_cleanup || snapshot.maintenance?.diagnostic
          ? settingsPanel('Room 删除清理', '逻辑删除已提交；隔离区中的受管数据仍需完成物理清理。',
            settingRow(
              `${snapshot.maintenance?.pending_cleanup || 0} 个待清理项`,
              snapshot.maintenance?.diagnostic || '可安全重试，不会恢复已删除 Room。',
              actionButton('重试清理', retryRoomDeletionCleanup, 'secondary-button')
            )
          )
          : null,
        snapshot.diagnostic ? node('aside', { className: 'callout danger' }, node('strong', { textContent: 'Registry diagnostic' }), node('span', { textContent: snapshot.diagnostic })) : null
      );
    }
    if (state.settingsSection === 'boundaries') {
      const caps = snapshot.capabilities || {};
      return node('div', { className: 'view-stack' },
        settingsPanel('Control-plane capabilities', '界面只呈现后端明确支持的控制能力。',
          capabilityRow('登记 canonical Project', true, '显式绝对路径；不提供服务端目录浏览。'),
          capabilityRow('导入 Legacy Room', caps.legacy_import !== false, '非破坏性登记，不搬移或重写 events.jsonl。'),
          capabilityRow('手动挂起 idle Runtime', caps.runtime_suspend === true, 'Busy Runtime 会拒绝操作。'),
          capabilityRow('热更新 Runtime 容量', caps.runtime_policy_mutation === true, 'Settings 中可调整最多同时活动的 Room Runtime。降低容量不会打断正在跑的 Turn。'),
          capabilityRow('应用内 Room surface', caps.room_surface === true, 'Management 同源网关承载应用内标签，不把 Runtime token 送到浏览器。'),
          capabilityRow('Room 生命周期管理', caps.room_deletion === true, '支持最多 100 个 Room 的批量归档与批量永久清理；永久清理仅接受已归档 Room，并要求一次不可恢复确认。显式外部导入目录只解绑并保留。'),
          capabilityRow('服务端路径浏览器', caps.server_path_browser === true, '避免扩大本机文件系统暴露面。')
        ),
        node('aside', { className: 'callout boundary' }, node('strong', { textContent: 'Transcript Boundary' }), node('span', { textContent: 'Vendor Transcript 与 PairRoom Room Event Log 是不同记录。Existing Binding 只恢复 vendor context，绑定前历史不会进入公共时间线。' })),
        node('aside', { className: 'callout warning' }, node('strong', { textContent: 'Binding Identity' }), node('span', { textContent: '(agent, vendor_session_id) 在整个 Service 内独占，包括已归档 Room。该约束防止同一 vendor transcript 被多个 Runtime 并发写入。' })),
        node('aside', { className: 'callout neutral' }, node('strong', { textContent: 'Browser session' }), node('span', { textContent: 'Management token 只用于一次性 bootstrap，随后换成 HttpOnly、SameSite=Strict 会话 Cookie；写操作还需要内存中的 CSRF token。' }))
      );
    }
    return node('div', { className: 'view-stack' },
      settingsPanel('外观', '主题与内嵌 Room 共享并持久化；其余偏好只在当前标签页生效。',
        settingRow('主题', '跟随系统，或临时固定浅色/深色。', segmented([
          ['system', '跟随系统'], ['light', '浅色'], ['dark', '深色'],
        ], state.preferences.theme, (value) => { state.preferences.theme = value; persistTheme(value); applyPreferences(); renderSettings(); }, '主题')),
        settingRow('信息密度', 'Compact 会缩小列表、表格和面板间距。', segmented([
          ['comfortable', '舒适'], ['compact', '紧凑'],
        ], state.preferences.density, (value) => { state.preferences.density = value; applyPreferences(); renderSettings(); }, '信息密度'))
      ),
      settingsPanel('刷新与导航', '控制当前页面如何轮询 Service。侧栏点击始终在应用内标签打开 Room。',
        settingRow('自动刷新', '页面隐藏时自动暂停，重新可见后立即同步。', selectControl([
          ['0', '关闭'], ['5000', '5 秒'], ['10000', '10 秒'], ['30000', '30 秒'], ['60000', '60 秒'],
        ], String(state.preferences.refreshMs), (value) => { state.preferences.refreshMs = Number(value); scheduleRefresh(); }, '自动刷新间隔')),
        settingRow('默认显示已归档', '影响 Projects 与 Project 详情列表。', toggleButton(state.filters.showArchived, (value) => { state.filters.showArchived = value; }, '切换归档可见性'))
      ),
      node('aside', { className: 'callout neutral' }, node('strong', { textContent: '无隐式持久化' }), node('span', { textContent: '这些界面选项不会写入 Service Registry，也不会改变 Room Event Log 或 Agent Session Binding。刷新标签页后恢复默认值。' }))
    );
  }

  function settingsPanel(title, subtitle, ...rows) {
    return node('section', { className: 'panel' },
      node('header', { className: 'panel-header' }, node('div', { className: 'panel-header-copy' }, node('h2', { textContent: title }), node('p', { textContent: subtitle }))),
      node('div', { className: 'setting-list' }, ...rows)
    );
  }

  function runtimeLimitControl(policy) {
    const mutable = state.snapshot?.capabilities?.runtime_policy_mutation === true;
    if (!mutable) return node('strong', { textContent: policy.limit ? String(policy.limit) : '未暴露' });
    const input = node('input', {
      type: 'number', min: '1', max: '128', value: String(policy.limit || 8),
      'aria-label': '最大同时活动 Room Runtime',
      onChange: async (event) => {
        const limit = Number(event.target.value);
        try {
          await api('/api/v1/runtime-policy', { method: 'PATCH', body: JSON.stringify({ limit }) });
          toast('Runtime 容量已更新', `最多同时 ${limit} 个活动 Runtime。`, 'success');
          await refresh({ forceRender: true });
        } catch (error) {
          toast('无法更新容量', error.message, 'error');
          event.target.value = String(policy.limit || 8);
        }
      },
    });
    return input;
  }

  function settingRow(title, description, control) {
    return node('div', { className: 'setting-row' }, node('div', { className: 'setting-copy' }, node('strong', { textContent: title }), node('p', { textContent: description })), node('div', { className: 'setting-control' }, control));
  }

  function capabilityRow(title, enabled, description) {
    return settingRow(title, description, statusBadge(enabled ? 'supported' : 'not supported', enabled ? 'good' : 'warn'));
  }

  function inlineCommand(command, successMessage) {
    return node('div', { className: 'inline-command' },
      node('code', { textContent: command }),
      actionButton('复制', () => copyText(command, successMessage), 'secondary-button compact-button')
    );
  }

  function attentionItems(snapshot) {
    const items = [];
    (snapshot.projects || []).forEach((project) => {
      if (!project.available) items.push({ kind: 'project', title: projectName(project), detail: project.diagnostic || 'Project root 不可访问。', symbol: '!', tone: 'danger', action: () => navigate(`#/projects/${encodeURIComponent(project.id)}`) });
    });
    (snapshot.rooms || []).forEach((room) => {
      if (roomHasBlockingPendingBindings(room)) {
        const project = projectForRoom(snapshot, room);
        items.push({ kind: 'binding', title: room.name, detail: `${projectName(project)} · Legacy Binding 待补全`, symbol: 'B', tone: 'warn', action: () => completeBindings(room) });
      }
    });
    runtimeModels(snapshot).forEach(({ room, runtime }) => {
      if (runtime.phase === 'failed') items.push({ kind: 'runtime', title: room.name, detail: runtime.last_error || 'Runtime 启动或关闭失败。', symbol: 'R', tone: 'danger', action: () => navigate('#/runtimes') });
    });
    const maintenance = snapshot.maintenance || {};
    if (maintenance.pending_cleanup || maintenance.diagnostic) {
      const count = Number(maintenance.pending_cleanup || 0);
      items.push({
        kind: 'maintenance',
        title: 'Room 数据清理待完成',
        detail: maintenance.diagnostic || `${count} 个隔离清理项可安全重试。`,
        symbol: 'D',
        tone: 'warn',
        action: () => { state.settingsSection = 'service'; navigate('#/settings'); },
      });
    }
    return items;
  }

  function renderAttentionItem(item) {
    return node('article', { className: 'list-item' },
      node('div', { className: 'list-main' }, node('div', { className: `item-symbol ${item.tone}`, textContent: item.symbol }), node('div', { className: 'list-copy' }, node('strong', { textContent: item.title }), node('p', { textContent: item.detail, title: item.detail }))),
      actionButton('处理', item.action, 'secondary-button compact-button')
    );
  }

  function liveRuntimeItems(snapshot) {
    return runtimeModels(snapshot).filter((item) => ['active', 'queued', 'starting', 'stopping'].includes(item.runtime.phase)).sort(compareRuntimeModels);
  }

  function renderLiveItem({ room, project, runtime }) {
    return node('article', { className: 'list-item' },
      node('div', { className: 'list-main' }, node('div', { className: `item-symbol ${runtime.busy ? 'warn' : 'accent'}`, textContent: runtime.busy ? '●' : '◎' }), node('div', { className: 'list-copy' }, node('strong', { textContent: room.name }), node('p', { textContent: `${projectName(project)} · ${runtimeLabel(runtime)}` }))),
      node('div', { className: 'list-meta' }, statusBadge(runtimeLabel(runtime), runtimeTone(runtime), runtime.busy ? 'busy' : ''), actionButton('打开', () => openRoom(room.id), 'secondary-button compact-button', roomHasBlockingPendingBindings(room)))
    );
  }

  function renderProjectOverviewItem(project) {
    const rooms = (state.snapshot.rooms || []).filter((room) => room.project_id === project.id);
    const active = rooms.filter((room) => room.lifecycle !== 'archived').length;
    const busy = rooms.filter((room) => getRuntime(room.id).busy).length;
    return node('article', { className: 'list-item' },
      node('div', { className: 'list-main' }, node('div', { className: 'item-symbol accent', textContent: projectInitials(project) }), node('div', { className: 'list-copy' }, node('strong', { textContent: projectName(project) }), node('p', { textContent: project.root, title: project.root }))),
      node('div', { className: 'list-meta' }, statusBadge(project.available ? 'available' : 'unavailable', project.available ? 'good' : 'danger'), node('span', { className: 'muted', textContent: `${active} rooms · ${busy} working` }), actionButton('查看', () => navigate(`#/projects/${encodeURIComponent(project.id)}`), 'secondary-button compact-button'))
    );
  }

  function buildProjectModels(snapshot) {
    const runtimeByRoom = new Map((snapshot.runtimes || []).map((runtime) => [runtime.room_id, runtime]));
    const roomsByProject = new Map();
    (snapshot.rooms || []).forEach((room) => {
      const values = roomsByProject.get(room.project_id) || [];
      values.push(room);
      roomsByProject.set(room.project_id, values);
    });
    return (snapshot.projects || []).map((project) => {
      const rooms = (roomsByProject.get(project.id) || []).sort((a, b) => Number(a.lifecycle === 'archived') - Number(b.lifecycle === 'archived') || a.name.localeCompare(b.name));
      const runtimes = rooms.map((room) => runtimeByRoom.get(room.id) || suspendedRuntime(room.id));
      return {
        project, rooms, runtimeByRoom,
        activeRooms: rooms.filter((room) => room.lifecycle !== 'archived').length,
        runtimeCounts: {
          active: runtimes.filter((runtime) => runtime.phase === 'active').length,
          busy: runtimes.filter((runtime) => runtime.busy).length,
          queued: runtimes.filter((runtime) => runtime.phase === 'queued').length,
          failed: runtimes.filter((runtime) => runtime.phase === 'failed').length,
        },
      };
    });
  }

  function projectMatchesFilters(model) {
    const { project, rooms } = model;
    if (state.filters.projectAvailability === 'available' && !project.available) return false;
    if (state.filters.projectAvailability === 'unavailable' && project.available) return false;
    const query = state.search.trim().toLocaleLowerCase();
    if (!query) return true;
    const haystack = [project.id, project.root, projectName(project), ...rooms.flatMap((room) => [room.id, room.name])].join('\n').toLocaleLowerCase();
    return haystack.includes(query);
  }

  function runtimeModels(snapshot) {
    const projects = new Map((snapshot.projects || []).map((project) => [project.id, project]));
    const runtimes = new Map((snapshot.runtimes || []).map((runtime) => [runtime.room_id, runtime]));
    return (snapshot.rooms || []).map((room) => ({ room, project: projects.get(room.project_id), runtime: runtimes.get(room.id) || suspendedRuntime(room.id) }));
  }

  function compareRuntimeModels(a, b) {
    const rank = { failed: 0, active: 1, starting: 2, queued: 3, stopping: 4, suspended: 5 };
    const phase = (rank[a.runtime.phase] ?? 9) - (rank[b.runtime.phase] ?? 9);
    if (phase) return phase;
    if (a.runtime.busy !== b.runtime.busy) return a.runtime.busy ? -1 : 1;
    if (a.runtime.queue_position || b.runtime.queue_position) return (a.runtime.queue_position || 9999) - (b.runtime.queue_position || 9999);
    return a.room.name.localeCompare(b.room.name);
  }

  function serviceSummary(snapshot) {
    if (!snapshot) return emptySummary();
    if (snapshot.summary) return { ...emptySummary(), ...snapshot.summary };
    const summary = emptySummary();
    summary.projects = (snapshot.projects || []).length;
    summary.unavailable_projects = (snapshot.projects || []).filter((project) => !project.available).length;
    summary.rooms = (snapshot.rooms || []).length;
    summary.active_rooms = (snapshot.rooms || []).filter((room) => room.lifecycle !== 'archived').length;
    summary.archived_rooms = summary.rooms - summary.active_rooms;
    summary.pending_bindings = (snapshot.rooms || []).filter(roomHasBlockingPendingBindings).length;
    summary.pending_room_cleanup = Number(snapshot.maintenance?.pending_cleanup || 0);
    (snapshot.runtimes || []).forEach((runtime) => {
      if (runtime.occupies_capacity || ['active', 'starting', 'stopping'].includes(runtime.phase)) summary.runtime_capacity_used++;
      if (runtime.phase === 'active') summary.active_runtimes++;
      if (runtime.busy) summary.busy_runtimes++;
      if (runtime.phase === 'queued') summary.queued_runtimes++;
      if (runtime.phase === 'failed') summary.failed_runtimes++;
    });
    const diagnosticOnlyCleanup = summary.pending_room_cleanup === 0 && Boolean(snapshot.maintenance?.diagnostic) ? 1 : 0;
    summary.attention_items = summary.unavailable_projects + summary.pending_bindings + summary.failed_runtimes + summary.pending_room_cleanup + diagnosticOnlyCleanup;
    return summary;
  }
  function emptySummary() {
    return { projects: 0, unavailable_projects: 0, rooms: 0, active_rooms: 0, archived_rooms: 0, pending_bindings: 0, pending_room_cleanup: 0, runtime_capacity_used: 0, active_runtimes: 0, busy_runtimes: 0, queued_runtimes: 0, failed_runtimes: 0, attention_items: 0 };
  }

  function runtimePolicy(snapshot) {
    return { limit: 0, idle_timeout_seconds: 0, poll_interval_milliseconds: 0, close_timeout_seconds: 0, ...(snapshot?.runtime_policy || {}) };
  }

  function getRuntime(roomID) {
    return state.snapshot?.runtimes?.find((runtime) => runtime.room_id === roomID) || suspendedRuntime(roomID);
  }

  function suspendedRuntime(roomID) {
    return { room_id: roomID, phase: 'suspended', busy: false, occupies_capacity: false };
  }

  function projectForRoom(snapshot, room) {
    return snapshot.projects?.find((project) => project.id === room.project_id);
  }

  function roomByID(roomID) {
    return state.snapshot?.rooms?.find((room) => room.id === roomID);
  }

  function projectName(project) {
    if (!project?.root) return '';
    const parts = project.root.split(/[\\/]/).filter(Boolean);
    return parts[parts.length - 1] || project.root;
  }

  function projectInitials(project) {
    const name = projectName(project);
    if (!name) return 'PR';
    const chunks = name.split(/[-_.\s]+/).filter(Boolean);
    return (chunks.length > 1 ? `${chunks[0][0]}${chunks[1][0]}` : name.slice(0, 2)).toUpperCase();
  }

  function roomHasBlockingPendingBindings(room) {
    return ['claude', 'codex'].some((actor) => {
      const binding = room.bindings?.[actor];
      return !binding || (binding.pending && binding.mode !== 'new');
    });
  }

  function bindingText(binding) {
    if (!binding) return 'missing';
    if (binding.pending && binding.mode === 'new') return `new · ${NEW_BINDING_HINT}`;
    if (binding.pending) return 'pending legacy binding';
    const id = String(binding.session_id || '');
    const compact = id.length > 24 ? `${id.slice(0, 10)}…${id.slice(-8)}` : id;
    return `${binding.mode || 'existing'}${compact ? ` · ${compact}` : ''}`;
  }

  function bindingMeta(actor, binding) {
    const title = bindingText(binding);
    return node('span', { className: 'binding-line', title }, node('span', { className: `agent-dot ${actor}`, textContent: actor === 'claude' ? 'C' : 'X' }), node('span', { textContent: title }));
  }

  function runtimeLabel(runtime) {
    if (runtime.phase === 'queued') return `queued #${runtime.queue_position || '?'}`;
    if (runtime.phase === 'active' && runtime.busy) return 'active · working';
    return runtime.phase || 'suspended';
  }

  function runtimeTone(runtime) {
    if (runtime.phase === 'failed') return 'danger';
    if (runtime.busy || runtime.phase === 'queued' || runtime.phase === 'stopping') return 'warn';
    if (runtime.phase === 'active') return 'good';
    if (runtime.phase === 'starting') return 'info';
    return '';
  }

  function openProjectDialog(mode = 'register') {
    setProjectMode(mode);
    $('project-path').value = '';
    hideFormError('project-form-error');
    showDialog('project-dialog');
    queueMicrotask(() => $('project-path').focus());
  }

  function setProjectMode(mode) {
    state.projectMode = mode;
    const importing = mode === 'import';
    $('project-mode-register').setAttribute('aria-selected', String(!importing));
    $('project-mode-import').setAttribute('aria-selected', String(importing));
    $('project-dialog-title').textContent = importing ? '导入 Legacy Room' : '登记 Project';
    $('project-dialog-subtitle').textContent = importing ? '显式登记自定义旧 data-dir。' : '添加 canonical Git worktree。';
    $('project-path').placeholder = importing ? '/absolute/path/to/legacy/room-data' : '/absolute/path/to/git/worktree';
    $('project-submit').textContent = importing ? '导入 Legacy Room' : '登记 Project';
    const help = $('project-mode-help');
    help.replaceChildren(
      node('strong', { textContent: importing ? '非破坏性导入' : '显式路径边界' }),
      node('span', { textContent: importing
        ? '不会搬移、复制或重写 events.jsonl；成功后同样参与 Project 与 Binding Identity 去重。'
        : '只接受绝对路径。根目录、子目录和符号链接会解析为同一个 canonical Git worktree；服务不会扫描开发目录。' })
    );
  }

  async function submitProject(event) {
    event.preventDefault();
    const path = $('project-path').value.trim();
    if (!looksAbsolutePath(path)) {
      showFormError('project-form-error', '请输入绝对路径；Service 不接受相对路径。');
      return;
    }
    const button = $('project-submit');
    await withBusy(button, async () => {
      try {
        hideFormError('project-form-error');
        const result = state.projectMode === 'import'
          ? await api('/api/v1/import', { method: 'POST', body: JSON.stringify({ path }) })
          : await api('/api/v1/projects', { method: 'POST', body: JSON.stringify({ path }) });
        closeDialog('project-dialog');
        toast(state.projectMode === 'import' ? 'Legacy Room 已导入' : 'Project 已登记', state.projectMode === 'import' ? '旧数据保持原位且未被重写。' : 'Canonical worktree 已加入 Service Registry。', 'success');
        await refresh({ forceRender: true });
        const projectID = state.projectMode === 'import' ? result.project_id : result.id;
        if (projectID) navigate(`#/projects/${encodeURIComponent(projectID)}`);
      } catch (error) {
        showFormError('project-form-error', error.message);
      }
    });
  }

  function openRoomDialog(projectID = '') {
    const projects = (state.snapshot?.projects || []).filter((project) => project.available);
    if (!projects.length) {
      toast('没有可用 Project', '请先登记或修复一个 Git worktree。', 'warning');
      return;
    }
    const select = $('room-project-id');
    select.replaceChildren(...projects.map((project) => node('option', { value: project.id, textContent: `${projectName(project)} — ${project.root}` })));
    select.value = projects.some((project) => project.id === projectID) ? projectID : projects[0].id;
    $('room-name').value = '';
    document.querySelector('input[name="claude-mode"][value="new"]').checked = true;
    document.querySelector('input[name="codex-mode"][value="new"]').checked = true;
    $('claude-session-id').value = '';
    $('codex-session-id').value = '';
    syncBindingInputs();
    hideFormError('room-form-error');
    $('room-dialog-title').textContent = `在 ${projectName(projects.find((project) => project.id === select.value))} 中创建 Room`;
    showDialog('room-dialog');
    queueMicrotask(() => $('room-name').focus());
  }

  function syncBindingInputs() {
    ['claude', 'codex'].forEach((actor) => {
      const mode = document.querySelector(`input[name="${actor}-mode"]:checked`)?.value || 'new';
      const input = $(`${actor}-session-id`);
      input.disabled = mode !== 'existing';
      input.required = mode === 'existing';
      if (mode !== 'existing') input.value = '';
    });
  }

  async function createRoom(event) {
    event.preventDefault();
    const projectID = $('room-project-id').value;
    const name = $('room-name').value.trim();
    if (!name) {
      showFormError('room-form-error', 'Room 名称不能为空。');
      return;
    }
    const bindings = {};
    for (const actor of ['claude', 'codex']) {
      const mode = document.querySelector(`input[name="${actor}-mode"]:checked`)?.value || 'new';
      const sessionID = $(`${actor}-session-id`).value.trim();
      if (mode === 'existing' && !sessionID) {
        showFormError('room-form-error', `${actor === 'claude' ? 'Claude Session' : 'Codex Thread'} ID 不能为空。`);
        return;
      }
      bindings[actor] = mode === 'existing' ? { mode, session_id: sessionID } : { mode };
    }
    await withBusy($('room-submit'), async () => {
      try {
        hideFormError('room-form-error');
        await api(`/api/v1/projects/${encodeURIComponent(projectID)}/rooms`, { method: 'POST', body: JSON.stringify({ name, bindings }) });
        closeDialog('room-dialog');
        toast('Room 已创建', 'Claude 与 Codex Binding 已完成原子验证。', 'success');
        await refresh({ forceRender: true });
        navigate(`#/projects/${encodeURIComponent(projectID)}`);
      } catch (error) {
        showFormError('room-form-error', error.message);
      }
    });
  }

  function openRenameDialog(room) {
    $('rename-room-id').value = room.id;
    $('rename-room-name').value = room.name;
    hideFormError('rename-form-error');
    showDialog('rename-dialog');
    queueMicrotask(() => { $('rename-room-name').focus(); $('rename-room-name').select(); });
  }

  async function submitRename(event) {
    event.preventDefault();
    const roomID = $('rename-room-id').value;
    const name = $('rename-room-name').value.trim();
    const room = roomByID(roomID);
    if (!name) {
      showFormError('rename-form-error', 'Room 名称不能为空。');
      return;
    }
    if (room && name === room.name) {
      closeDialog('rename-dialog');
      return;
    }
    const button = event.submitter || event.currentTarget.querySelector('[type="submit"]');
    await withBusy(button, async () => {
      try {
        await api(`/api/v1/rooms/${encodeURIComponent(roomID)}`, { method: 'PATCH', body: JSON.stringify({ name }) });
        closeDialog('rename-dialog');
        toast('Room 已重命名', '变更在安全 Turn 边界提交。', 'success');
        await refresh({ forceRender: true });
      } catch (error) {
        showFormError('rename-form-error', error.message);
      }
    });
  }

  function completeBindings(room) {
    state.bindingRoomID = room.id;
    const container = $('binding-fields');
    container.replaceChildren();
    for (const actor of ['claude', 'codex']) {
      const binding = room.bindings?.[actor];
      if (binding && (!binding.pending || binding.mode === 'new')) continue;
      const label = actor === 'claude' ? 'Claude Session' : 'Codex Thread';
      const field = node('fieldset', { className: 'binding-card', 'data-complete-actor': actor },
        node('legend', {}, node('span', { className: `agent-avatar ${actor}`, textContent: actor === 'claude' ? 'C' : 'X' }), node('span', {}, node('strong', { textContent: label }), node('small', { textContent: 'Missing Binding' }))),
        node('label', { className: 'choice-card' }, node('input', { type: 'radio', name: `complete-${actor}-mode`, value: 'new', checked: true }), node('span', {}, node('strong', { textContent: `新建 ${label}` }), node('small', { textContent: 'Identity 在首次真实 Turn 上固化' }))),
        node('label', { className: 'choice-card' }, node('input', { type: 'radio', name: `complete-${actor}-mode`, value: 'existing' }), node('span', {}, node('strong', { textContent: '复用 Existing' }), node('small', { textContent: '必须未被其他 Room 占用' }))),
        node('label', { className: 'session-field' }, node('span', { textContent: `${label} ID` }), node('input', { id: `complete-${actor}-session`, type: 'text', placeholder: `粘贴 ${label} ID`, autocomplete: 'off', disabled: true }))
      );
      field.querySelectorAll('input[type="radio"]').forEach((radio) => radio.addEventListener('change', () => {
        const existing = field.querySelector('input[type="radio"][value="existing"]').checked;
        const input = field.querySelector('input[type="text"]');
        input.disabled = !existing;
        input.required = existing;
        if (!existing) input.value = '';
      }));
      container.append(field);
    }
    container.append(node('div', { className: 'callout boundary' }, node('strong', { textContent: 'Atomic completion' }), node('span', { textContent: '任一 Existing ID 无效、不可恢复或已被占用时，整个补全过程失败且不留下部分占用。' })));
    hideFormError('binding-form-error');
    showDialog('binding-dialog');
  }

  async function submitBindingCompletion(event) {
    event.preventDefault();
    const bindings = {};
    for (const actor of ['claude', 'codex']) {
      const modeNode = document.querySelector(`input[name="complete-${actor}-mode"]:checked`);
      if (!modeNode) continue;
      const mode = modeNode.value;
      const input = $(`complete-${actor}-session`);
      const sessionID = input?.value.trim() || '';
      if (mode === 'existing' && !sessionID) {
        showFormError('binding-form-error', `${actor === 'claude' ? 'Claude Session' : 'Codex Thread'} ID 不能为空。`);
        return;
      }
      bindings[actor] = mode === 'existing' ? { mode, session_id: sessionID } : { mode };
    }
    const button = event.submitter || event.currentTarget.querySelector('[type="submit"]');
    await withBusy(button, async () => {
      try {
        await api(`/api/v1/rooms/${encodeURIComponent(state.bindingRoomID)}/bindings`, { method: 'POST', body: JSON.stringify({ bindings }) });
        closeDialog('binding-dialog');
        toast('Binding 已补全', 'Legacy Room 现在可以安全激活。', 'success');
        await refresh({ forceRender: true });
      } catch (error) {
        showFormError('binding-form-error', error.message);
      }
    });
  }

  function archiveRoom(room) {
    openConfirm({
      eyebrow: 'ARCHIVE ROOM',
      title: `归档“${room.name}”？`,
      message: '活动 Turn 会先被停止，Runtime 随后挂起；Room 将从默认列表隐藏。',
      detail: 'Event Log、附件、角色、草稿、未读状态和两侧 Binding Identity 都会完整保留。',
      label: '归档 Room',
      tone: 'danger',
      action: async () => {
        await api(`/api/v1/rooms/${encodeURIComponent(room.id)}/archive`, { method: 'POST' });
        toast('Room 已归档', '历史与 Binding Identity 已保留。', 'success');
        await refresh({ forceRender: true });
      },
    });
  }

  async function refreshProject(project) {
    try {
      const refreshed = await api(`/api/v1/projects/${encodeURIComponent(project.id)}/refresh`, { method: 'POST' });
      if (refreshed.available) {
        toast('Project 可用', 'Canonical worktree 已重新验证。', 'success');
      } else {
        toast('Project 仍不可用', refreshed.diagnostic || 'Canonical worktree 当前无法访问。', 'warning');
      }
      await refresh({ forceRender: true });
    } catch (error) {
      toast('Project 检查失败', error.message, 'error');
    }
  }
  function uniqueRooms(candidates = state.snapshot?.rooms || []) {
    const seen = new Set();
    return (candidates || []).filter((room) => {
      if (!room || !room.id || seen.has(room.id)) return false;
      seen.add(room.id);
      return true;
    });
  }
  function eligibleActiveRooms(candidates = state.snapshot?.rooms || []) {
    return uniqueRooms(candidates).filter((room) => room.lifecycle !== 'archived');
  }
  function eligibleArchivedRooms(candidates = state.snapshot?.rooms || []) {
    return uniqueRooms(candidates).filter((room) => room.lifecycle === 'archived');
  }
  function visibleRoomsForBatch(candidates = state.snapshot?.rooms || []) {
    return uniqueRooms(candidates).filter((room) => state.filters.showArchived || room.lifecycle !== 'archived');
  }
  function pruneRoomSelection(snapshot = state.snapshot) {
    const roomIDs = new Set(uniqueRooms(snapshot?.rooms || []).map((room) => room.id));
    for (const roomID of state.selectedRoomIDs) {
      if (!roomIDs.has(roomID)) state.selectedRoomIDs.delete(roomID);
    }
  }
  function selectedActiveRooms(candidates = state.snapshot?.rooms || []) {
    return eligibleActiveRooms(candidates).filter((room) => state.selectedRoomIDs.has(room.id));
  }
  function selectedArchivedRooms(candidates = state.snapshot?.rooms || []) {
    return eligibleArchivedRooms(candidates).filter((room) => state.selectedRoomIDs.has(room.id));
  }
  function toggleRoomSelection(roomID, selected) {
    if (selected && !state.selectedRoomIDs.has(roomID) && state.selectedRoomIDs.size >= MAX_ROOM_BATCH_SIZE) {
      toast('已达到批量上限', `每次最多选择 ${MAX_ROOM_BATCH_SIZE} 个 Room。`, 'warning');
      render();
      return;
    }
    if (selected) state.selectedRoomIDs.add(roomID);
    else state.selectedRoomIDs.delete(roomID);
    render();
  }
  function toggleRoomSelectionGroup(candidates) {
    const eligible = uniqueRooms(candidates);
    const allSelected = eligible.length > 0 && eligible.every((room) => state.selectedRoomIDs.has(room.id));
    if (allSelected) {
      eligible.forEach((room) => state.selectedRoomIDs.delete(room.id));
      render();
      return;
    }
    let skipped = 0;
    for (const room of eligible) {
      if (state.selectedRoomIDs.has(room.id)) continue;
      if (state.selectedRoomIDs.size >= MAX_ROOM_BATCH_SIZE) {
        skipped++;
        continue;
      }
      state.selectedRoomIDs.add(room.id);
    }
    if (skipped) toast('部分 Room 未选择', `每次最多处理 ${MAX_ROOM_BATCH_SIZE} 个；还有 ${skipped} 个可在下一批处理。`, 'warning');
    render();
  }
  function roomSelectionToggleButton(candidates, scopeLabel) {
    const eligible = uniqueRooms(candidates);
    const selected = eligible.filter((room) => state.selectedRoomIDs.has(room.id));
    const allSelected = eligible.length > 0 && selected.length === eligible.length;
    const label = allSelected
      ? `取消选择${scopeLabel}中的 Room (${selected.length})`
      : `选择${scopeLabel}中的 Room (${eligible.length})`;
    return actionButton(label, () => toggleRoomSelectionGroup(eligible), `filter-chip ${selected.length ? 'active' : ''}`, eligible.length === 0);
  }
  function roomClearSelectionButton() {
    const count = state.selectedRoomIDs.size;
    if (!count) return null;
    return actionButton(`清除选择 (${count})`, () => {
      state.selectedRoomIDs.clear();
      render();
    }, 'secondary-button outline');
  }
  function roomBatchArchiveButton(rooms) {
    const count = rooms.length;
    return actionButton(`批量归档 (${count})`, () => confirmRoomArchive(rooms), 'secondary-button outline', count === 0);
  }
  function roomBatchRemovalButton(rooms) {
    const count = rooms.length;
    return actionButton(`批量清理 (${count})`, () => confirmRoomRemoval(rooms), 'danger-button outline', count === 0);
  }
  function projectRemovalButton(project, roomCount, compact = false) {
    if (!state.snapshot?.capabilities?.project_removal) return null;
    const disabled = roomCount > 0;
    const explanation = disabled
      ? `仍包含 ${roomCount} 个 Room（含已归档）；请先归档并永久清除。`
      : '只移除 Service Registry 登记，不删除 Git worktree 或外部数据。';
    return node('button', {
      type: 'button',
      className: `danger-button outline${compact ? ' compact-button' : ''}`,
      textContent: '注销 Project',
      disabled,
      title: explanation,
      'aria-label': `注销 Project ${projectName(project)}。${explanation}`,
      onClick: () => removeProject(project),
    });
  }
  function removeProject(project) {
    const roomCount = (state.snapshot?.rooms || []).filter((room) => room.project_id === project.id).length;
    if (roomCount > 0) {
      toast('不能注销 Project', `仍包含 ${roomCount} 个 Room（含已归档）；请先归档并永久清除。`, 'warning');
      return;
    }
    openConfirm({
      eyebrow: 'UNREGISTER PROJECT',
      title: `注销“${projectName(project)}”？`,
      message: '将此空 Project 从 Service Registry 注销。',
      detail: '不会删除 Git worktree 或 vendor Session/Thread。后端仍会最终复检 Project 确实不含任何 Room。',
      label: '注销 Project',
      tone: 'danger',
      confirmation: project.id,
      confirmationLabel: '输入完整 Project ID 以确认',
      action: async () => {
        await api(`/api/v1/projects/${encodeURIComponent(project.id)}`, {
          method: 'DELETE',
          body: JSON.stringify({ confirm_project_id: project.id }),
        });
        toast('Project 已注销', 'Git worktree 与外部数据未被修改。', 'success');
        await refresh({ forceRender: true });
        navigate('#/projects');
      },
    });
  }
  function confirmRoomArchive(candidates) {
    const rooms = eligibleActiveRooms(candidates);
    if (!rooms.length) {
      toast('没有可归档的 Room', '请选择至少一个活跃 Room。', 'warning');
      return;
    }
    if (rooms.length > MAX_ROOM_BATCH_SIZE) {
      toast('超过批量上限', `每次最多处理 ${MAX_ROOM_BATCH_SIZE} 个 Room。`, 'warning');
      return;
    }
    const preview = rooms.slice(0, 6).map((room) => room.name).join('、');
    const remaining = rooms.length > 6 ? `，另有 ${rooms.length - 6} 个` : '';
    openConfirm({
      eyebrow: rooms.length === 1 ? 'ARCHIVE ROOM' : 'BATCH ARCHIVE ROOMS',
      title: rooms.length === 1 ? `归档“${rooms[0].name}”？` : `归档 ${rooms.length} 个 Room？`,
      message: `${preview}${remaining}。归档后仍可恢复，也可继续批量永久清理。`,
      detail: '归档保留 Event Log、附件和 Agent Binding。批量请求逐项执行；忙碌 Room 的活动 Turn 会先被停止，失败项保持选中，可稍后重试。',
      label: rooms.length === 1 ? '归档 Room' : `归档 ${rooms.length} 个 Room`,
      tone: 'primary',
      action: async () => {
        const response = await api('/api/v1/rooms/batch-archive', {
          method: 'POST',
          body: JSON.stringify({ room_ids: rooms.map((room) => room.id) }),
        });
        if (!Array.isArray(response.results)) throw new Error('批量归档响应缺少 results。');
        const results = response.results;
        const succeeded = results.filter((item) => item.status === 'archived' || item.status === 'already_archived');
        const failed = results.filter((item) => item.status !== 'archived' && item.status !== 'already_archived');
        succeeded.forEach((item) => state.selectedRoomIDs.add(item.room_id));
        if (succeeded.length) state.filters.showArchived = true;
        if (failed.length) {
          const detail = failed.slice(0, 2).map((item) => {
            const room = rooms.find((candidate) => candidate.id === item.room_id);
            return `${room?.name || item.room_id}: ${item.error || item.code || '归档失败'}`;
          }).join('；');
          const more = failed.length > 2 ? `；另有 ${failed.length - 2} 个失败` : '';
          toast(
            succeeded.length ? '批量归档部分完成' : '批量归档未完成',
            `成功 ${succeeded.length} 个，失败 ${failed.length} 个。${detail}${more}`,
            succeeded.length ? 'warning' : 'error'
          );
        } else {
          toast('Room 已归档', `已归档 ${succeeded.length} 个 Room；所选项保持选中，可直接继续批量清理。`, 'success');
        }
        await refresh({ forceRender: true });
      },
    });
  }
  function confirmRoomRemoval(candidates) {
    const rooms = eligibleArchivedRooms(candidates);
    if (!rooms.length) {
      toast('没有可清理的 Room', '请选择至少一个已归档 Room。', 'warning');
      return;
    }
    if (rooms.length > MAX_ROOM_BATCH_SIZE) {
      toast('超过批量上限', `每次最多处理 ${MAX_ROOM_BATCH_SIZE} 个 Room。`, 'warning');
      return;
    }
    const preview = rooms.slice(0, 6).map((room) => room.name).join('、');
    const remaining = rooms.length > 6 ? `，另有 ${rooms.length - 6} 个` : '';
    openConfirm({
      eyebrow: rooms.length === 1 ? 'PERMANENTLY REMOVE ROOM' : 'BATCH REMOVE ROOMS',
      title: rooms.length === 1 ? `永久清除“${rooms[0].name}”？` : `永久清除 ${rooms.length} 个 Room？`,
      message: `${preview}${remaining}。此操作不可撤销。`,
      detail: 'PairRoom 管理的 Event Log、附件和 Room 数据会被删除；Git worktree 与 vendor Claude Session/Codex Thread 不会被删除。显式导入的外部目录只解绑并保留。批量请求逐项执行，失败项不会回滚已完成项。',
      label: rooms.length === 1 ? '永久清除 Room' : `永久清除 ${rooms.length} 个 Room`,
      tone: 'danger',
      acknowledgement: `我理解所选 ${rooms.length} 个 Room 的 PairRoom 管理数据将永久删除且无法恢复。`,
      action: async () => {
        const response = await api('/api/v1/rooms/batch-delete', {
          method: 'POST',
          body: JSON.stringify({
            room_ids: rooms.map((room) => room.id),
            acknowledge_data_loss: true,
          }),
        });
        if (!Array.isArray(response.results)) throw new Error('批量清理响应缺少 results。');
        const results = response.results;
        const succeeded = results.filter((item) => item.status === 'deleted');
        const failed = results.filter((item) => item.status !== 'deleted');
        succeeded.forEach((item) => state.selectedRoomIDs.delete(item.room_id));
        failed.forEach((item) => {
          if (state.selectedRoomIDs.has(item.room_id) || state.selectedRoomIDs.size < MAX_ROOM_BATCH_SIZE) {
            state.selectedRoomIDs.add(item.room_id);
          }
        });
        const cleanupPending = succeeded.filter((item) => item.removal?.data_disposition === 'cleanup_pending').length;
        const retainedExternal = succeeded.filter((item) => item.removal?.data_disposition === 'retained_external').length;
        if (failed.length) {
          const detail = failed.slice(0, 2).map((item) => {
            const room = rooms.find((candidate) => candidate.id === item.room_id);
            return `${room?.name || item.room_id}: ${item.error || item.code || '删除失败'}`;
          }).join('；');
          const more = failed.length > 2 ? `；另有 ${failed.length - 2} 个失败` : '';
          toast(
            succeeded.length ? '批量清理部分完成' : '批量清理未完成',
            `成功 ${succeeded.length} 个，失败 ${failed.length} 个。${detail}${more}`,
            succeeded.length ? 'warning' : 'error'
          );
        } else if (cleanupPending) {
          toast('Room 已移除，物理清理待重试', `已处理 ${succeeded.length} 个；其中 ${cleanupPending} 个隔离清理项可在设置中重试。`, 'warning');
        } else if (retainedExternal) {
          toast('Room 清理完成', `已处理 ${succeeded.length} 个；其中 ${retainedExternal} 个外部导入目录保持原位。`, 'success');
        } else {
          toast('Room 已永久清除', `已成功清理 ${succeeded.length} 个已归档 Room。`, 'success');
        }
        await refresh({ forceRender: true });
      },
    });
  }
  async function retryRoomDeletionCleanup() {
    try {
      const maintenance = await api('/api/v1/maintenance/room-deletions/retry', { method: 'POST' });
      if (maintenance.pending_cleanup) {
        toast('仍有清理项', maintenance.diagnostic || `${maintenance.pending_cleanup} 个隔离项仍待处理。`, 'warning');
      } else {
        toast('Room 清理已完成', '删除隔离区已清空。', 'success');
      }
      await refresh({ forceRender: true });
    } catch (error) {
      toast('Room 清理重试失败', error.message, 'error');
    }
  }
  async function restoreRoom(room) {
    try {
      await api(`/api/v1/rooms/${encodeURIComponent(room.id)}/restore`, { method: 'POST' });
      toast('Room 已恢复', '完整历史与 Binding Identity 可再次使用。', 'success');
      await refresh({ forceRender: true });
    } catch (error) {
      toast('恢复失败', error.message, 'error');
    }
  }

  function suspendRoom(room, runtime) {
    const queued = runtime.phase === 'queued';
    openConfirm({
      eyebrow: queued ? 'CANCEL ACTIVATION' : 'SUSPEND RUNTIME',
      title: queued ? `取消“${room.name}”的激活排队？` : `挂起“${room.name}”的 Runtime？`,
      message: queued ? 'Room 会回到 Suspended；下次打开时可重新进入队列。' : '只有没有活动 Turn 的 Runtime 才会关闭 vendor 进程并释放容量。',
      detail: queued ? '' : '若 Room 正在工作，后端会返回冲突且不会发送 Interrupt。Room 历史和 Session/Thread Binding 不受影响。',
      label: queued ? '取消排队' : '挂起 Runtime',
      tone: 'danger',
      action: async () => {
        await api(`/api/v1/rooms/${encodeURIComponent(room.id)}/suspend`, { method: 'POST' });
        toast(queued ? '已取消排队' : 'Runtime 已挂起', queued ? 'Room 仍保持可恢复。' : '容量已安全释放。', 'success');
        await refresh({ forceRender: true });
      },
    });
  }

  async function openRoom(roomID) {
    const room = roomByID(roomID);
    if (!room) return;
    if (room.lifecycle === 'archived') {
      toast('Room 已归档', '恢复后才能打开。', 'warning');
      return;
    }
    if (roomHasBlockingPendingBindings(room)) {
      completeBindings(room);
      return;
    }
    if (!state.tabs.includes(roomID)) state.tabs.push(roomID);
    if (location.hash !== `#/rooms/${encodeURIComponent(roomID)}`) navigate(`#/rooms/${encodeURIComponent(roomID)}`);
    else {
      state.route = parseRoute();
      syncRoomTabs();
    }
    await activateRoomRuntime(roomID);
  }

  async function openRoomInBrowserAction(roomID) {
    const room = roomByID(roomID);
    if (!room || room.lifecycle === 'archived') {
      toast('无法在浏览器中打开', '归档 Room 没有独立 Runtime URL，请先恢复。', 'warning');
      return;
    }
    if (roomHasBlockingPendingBindings(room)) {
      completeBindings(room);
      return;
    }
    const deadline = Date.now() + 60000;
    toast('正在准备系统浏览器', '等待 Runtime 就绪后再打开。', 'success');
    try {
      while (Date.now() < deadline) {
        const status = await api(`/api/v1/rooms/${encodeURIComponent(roomID)}/activate`, { method: 'POST' });
        if (status.phase === 'active' && status.url) break;
        await new Promise((resolve) => setTimeout(resolve, 400));
        await refresh();
      }
      const runtime = getRuntime(roomID);
      if (runtime.phase !== 'active' || !runtime.url) {
        throw new Error('等待 Runtime 超时');
      }
      await api(`/api/v1/rooms/${encodeURIComponent(roomID)}/open-browser`, { method: 'POST' });
      toast('已在系统浏览器中打开', '不会离开当前工作台。', 'success');
    } catch (error) {
      const runtime = getRuntime(roomID);
      if (runtime.url) await copyText(runtime.url, '已复制一次性 Room URL，可粘贴到系统浏览器。');
      toast('无法打开系统浏览器', error.message, 'error');
    }
  }

  function resetConfirmState() {
    state.confirmAction = null;
    state.confirmRequirement = '';
    state.confirmAcknowledgementRequired = false;
    const wrapper = $('confirm-input-wrap');
    const expected = $('confirm-expected');
    const input = $('confirm-input');
    const inputLabel = $('confirm-input-label');
    const acknowledgementWrapper = $('confirm-ack-wrap');
    const acknowledgement = $('confirm-ack');
    const acknowledgementLabel = $('confirm-ack-label');
    if (wrapper) wrapper.hidden = true;
    if (expected) expected.textContent = '';
    if (inputLabel) inputLabel.textContent = '输入完整 ID 以确认';
    if (input) {
      input.value = '';
      input.required = false;
      input.setCustomValidity('');
    }
    if (acknowledgementWrapper) acknowledgementWrapper.hidden = true;
    if (acknowledgementLabel) acknowledgementLabel.textContent = '我理解此操作不可恢复。';
    if (acknowledgement) {
      acknowledgement.checked = false;
      acknowledgement.required = false;
      acknowledgement.setCustomValidity('');
    }
    if ($('confirm-submit')) $('confirm-submit').disabled = false;
  }
  function syncConfirmRequirement() {
    const input = $('confirm-input');
    const acknowledgement = $('confirm-ack');
    const submit = $('confirm-submit');
    if (!input || !acknowledgement || !submit) return;
    const matches = !state.confirmRequirement || input.value === state.confirmRequirement;
    const acknowledged = !state.confirmAcknowledgementRequired || acknowledgement.checked;
    input.setCustomValidity(matches ? '' : '请输入完整且逐字匹配的 ID。');
    acknowledgement.setCustomValidity(acknowledged ? '' : '请先确认你理解此操作不可恢复。');
    submit.disabled = !matches || !acknowledged;
  }
  function openConfirm({ eyebrow = 'CONFIRM', title, message, detail = '', label = '确认', tone = 'danger', confirmation = '', confirmationLabel = '输入完整 ID 以确认', acknowledgement = '', action }) {
    resetConfirmState();
    state.confirmAction = action;
    state.confirmRequirement = confirmation;
    state.confirmAcknowledgementRequired = Boolean(acknowledgement);
    $('confirm-eyebrow').textContent = eyebrow;
    $('confirm-title').textContent = title;
    $('confirm-message').textContent = message;
    const detailNode = $('confirm-detail');
    detailNode.hidden = !detail;
    detailNode.replaceChildren();
    if (detail) detailNode.append(node('strong', { textContent: '安全边界' }), node('span', { textContent: detail }));
    const requirement = $('confirm-input-wrap');
    const input = $('confirm-input');
    const acknowledgementWrapper = $('confirm-ack-wrap');
    const acknowledgementInput = $('confirm-ack');
    requirement.hidden = !confirmation;
    $('confirm-input-label').textContent = confirmationLabel;
    $('confirm-expected').textContent = confirmation;
    input.required = Boolean(confirmation);
    input.value = '';
    acknowledgementWrapper.hidden = !acknowledgement;
    $('confirm-ack-label').textContent = acknowledgement || '我理解此操作不可恢复。';
    acknowledgementInput.required = Boolean(acknowledgement);
    acknowledgementInput.checked = false;
    $('confirm-submit').textContent = label;
    $('confirm-submit').className = tone === 'danger' ? 'danger-button' : 'primary-button';
    syncConfirmRequirement();
    showDialog('confirm-dialog');
    queueMicrotask(() => (confirmation ? input : (acknowledgement ? acknowledgementInput : $('confirm-submit'))).focus());
  }
  async function submitConfirm(event) {
    event.preventDefault();
    const action = state.confirmAction;
    if (!action) {
      closeDialog('confirm-dialog');
      return;
    }
    syncConfirmRequirement();
    if (state.confirmRequirement && $('confirm-input').value !== state.confirmRequirement) {
      $('confirm-input').reportValidity();
      return;
    }
    if (state.confirmAcknowledgementRequired && !$('confirm-ack').checked) {
      $('confirm-ack').reportValidity();
      return;
    }
    await withBusy($('confirm-submit'), async () => {
      try {
        await action();
        closeDialog('confirm-dialog');
      } catch (error) {
        toast('操作失败', error.message, 'error');
      }
    });
  }

  function showDialog(id) {
    document.querySelectorAll('dialog[open]').forEach((dialog) => {
      if (dialog.id !== id) dialog.close();
    });
    const dialog = $(id);
    if (!dialog.open) dialog.showModal();
  }

  function closeDialog(id) {
    const dialog = $(id);
    if (dialog?.open) dialog.close();
    if (id === 'confirm-dialog') resetConfirmState();
    if (state.renderPending && canRenderNow()) {
      render();
      state.renderPending = false;
    }
  }

  function applyPreferences() {
    if (state.preferences.theme === 'system') delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = state.preferences.theme;
    document.body.dataset.density = state.preferences.density;
  }

  function runtimeFlags(policy) {
    const limit = policy.limit || 2;
    const idle = policy.idle_timeout_seconds || 900;
    const idleValue = idle % 60 === 0 ? `${idle / 60}m` : `${idle}s`;
    return `--runtime-limit ${limit} --idle-timeout ${idleValue}`;
  }

  function runtimeCommand(policy) {
    return `pairroom service ${runtimeFlags(policy)}`;
  }

  function daemonInstallCommand(policy) {
    return `pairroom daemon install --force -- ${runtimeFlags(policy)}`;
  }

  function diagnosticSnapshot() {
    const clone = JSON.parse(JSON.stringify(state.snapshot || {}));
    (clone.runtimes || []).forEach((runtime) => {
      if (runtime.url) runtime.url = '[redacted local runtime URL]';
    });
    clone.management = {
      exported_at: new Date().toISOString(),
      route: state.route.name,
      connected: state.connected,
      note: 'Room runtime URLs are redacted. Local filesystem paths remain present.',
    };
    return clone;
  }

  function downloadDiagnosticSnapshot() {
    const data = JSON.stringify(diagnosticSnapshot(), null, 2);
    const blob = new Blob([data], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = `pairroom-service-diagnostics-${new Date().toISOString().replace(/[:.]/g, '-')}.json`;
    document.body.append(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 0);
    toast('诊断已导出', 'Runtime URL 已脱敏；本机路径仍可能敏感。', 'success');
  }

  async function copyText(value, successMessage = '已复制。') {
    try {
      await navigator.clipboard.writeText(String(value));
      toast('复制成功', successMessage, 'success');
    } catch {
      const input = document.createElement('textarea');
      input.value = String(value);
      input.setAttribute('readonly', '');
      input.style.cssText = 'position:fixed;opacity:0;pointer-events:none';
      document.body.append(input);
      input.select();
      const copied = document.execCommand('copy');
      input.remove();
      toast(copied ? '复制成功' : '复制失败', copied ? successMessage : '浏览器拒绝了剪贴板操作。', copied ? 'success' : 'error');
    }
  }

  function looksAbsolutePath(value) {
    return value.startsWith('/') || /^[A-Za-z]:[\\/]/.test(value) || /^\\\\[^\\]+\\[^\\]+/.test(value);
  }

  async function withBusy(button, work) {
    if (!button || button.disabled) return;
    const original = button.textContent;
    button.disabled = true;
    button.textContent = '处理中…';
    try { await work(); } finally { button.disabled = false; button.textContent = original; }
  }

  function showFormError(id, message) {
    const target = $(id);
    target.textContent = message;
    target.hidden = false;
  }

  function hideFormError(id) {
    const target = $(id);
    target.textContent = '';
    target.hidden = true;
  }

  function node(tag, properties = {}, ...children) {
    const element = document.createElement(tag);
    for (const [key, value] of Object.entries(properties || {})) {
      if (value === undefined || value === null || value === false) continue;
      if (key === 'className') element.className = value;
      else if (key === 'textContent') element.textContent = value;
      else if (key === 'style') element.setAttribute('style', value);
      else if (key.startsWith('on') && typeof value === 'function') element.addEventListener(key.slice(2).toLowerCase(), value);
      else if (key === 'checked') element.checked = Boolean(value);
      else if (key === 'disabled') element.disabled = Boolean(value);
      else if (key === 'value') element.value = value;
      else element.setAttribute(key, String(value));
    }
    for (const child of children.flat(Infinity)) {
      if (child === undefined || child === null || child === false || child === '') continue;
      element.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return element;
  }

  function actionButton(label, handler, className = 'secondary-button', disabled = false) {
    return node('button', { type: 'button', className, textContent: label, disabled, onClick: handler });
  }

  function statusBadge(label, tone = '', extra = '') {
    return node('span', { className: `badge ${tone} ${extra}`.trim(), textContent: label });
  }

  function statCard(label, value, hint, symbol, tone = '', onClick = null) {
    const card = node(onClick ? 'button' : 'article', { className: `panel stat-card ${tone}`.trim(), ...(onClick ? { type: 'button', onClick } : {}) },
      node('div', { className: 'stat-top' }, node('span', { className: 'stat-label', textContent: label }), node('span', { className: 'stat-icon', textContent: symbol, 'aria-hidden': 'true' })),
      node('div', {}, node('div', { className: 'stat-value', textContent: String(value) }), node('div', { className: 'stat-hint', textContent: hint }))
    );
    if (onClick) card.setAttribute('aria-label', `${label}: ${value}. ${hint}`);
    return card;
  }

  function panel(title, subtitle, body, footer = '', action = null) {
    return node('section', { className: 'panel' },
      node('header', { className: 'panel-header' }, node('div', { className: 'panel-header-copy' }, node('h2', { textContent: title }), subtitle ? node('p', { textContent: subtitle }) : null), action),
      body,
      footer ? node('footer', { className: 'panel-footer', textContent: footer }) : null
    );
  }

  function emptyState(symbol, title, description, compact = false, action = null) {
    return node('div', { className: `empty-state ${compact ? 'compact' : ''}` }, node('div', {}, node('div', { className: 'empty-symbol', textContent: symbol, 'aria-hidden': 'true' }), node('h3', { textContent: title }), node('p', { textContent: description }), action));
  }

  function heroMeta(label, value) {
    return node('div', { className: 'hero-meta-item' }, node('span', { textContent: label }), node('strong', { textContent: value }));
  }

  function summaryCell(value, label) {
    return node('div', { className: 'project-summary-item' }, node('strong', { textContent: String(value) }), node('span', { textContent: label }));
  }

  function keyValue(label, value, mono = false) {
    return node('div', { className: 'key-value' }, node('span', { textContent: label }), node(mono ? 'code' : 'strong', { textContent: value || '—' }));
  }

  function selectControl(options, value, onChange, ariaLabel) {
    const select = node('select', { value, 'aria-label': ariaLabel, onChange: (event) => onChange(event.target.value) }, ...options.map(([optionValue, label]) => node('option', { value: optionValue, textContent: label })));
    select.value = value;
    return select;
  }

  function segmented(options, value, onChange, ariaLabel = '') {
    const group = node('div', { className: 'segmented-control', role: 'group' }, ...options.map(([optionValue, label]) => {
      const active = value === optionValue;
      const button = actionButton(label, () => onChange(optionValue), active ? 'active' : '');
      button.setAttribute('aria-pressed', String(active));
      return button;
    }));
    if (ariaLabel) group.setAttribute('aria-label', ariaLabel);
    return group;
  }

  function toggleButton(value, onChange, label) {
    return node('button', { type: 'button', className: 'toggle-switch', role: 'switch', 'aria-checked': String(value), 'aria-label': label, onClick: () => { onChange(!value); if (state.route.name === 'settings') renderSettings(); } });
  }

  function formatDateTime(value) {
    if (!value) return '—';
    const date = new Date(value);
    const time = date.getTime();
    if (Number.isNaN(time) || time <= 0) return '—';
    return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'medium' }).format(date);
  }

  function formatRelativeTime(value) {
    if (!value) return '—';
    const date = new Date(value);
    const time = date.getTime();
    if (Number.isNaN(time) || time <= 0) return '—';
    const delta = Date.now() - time;
    if (!Number.isFinite(delta)) return String(value);
    const abs = Math.abs(delta);
    if (abs < 5000) return '刚刚';
    if (abs < 60000) return `${Math.round(abs / 1000)} 秒前`;
    if (abs < 3600000) return `${Math.round(abs / 60000)} 分钟前`;
    if (abs < 86400000) return `${Math.round(abs / 3600000)} 小时前`;
    return formatDateTime(value);
  }

  function formatDuration(seconds) {
    const value = Number(seconds);
    if (!Number.isFinite(value) || value <= 0) return '—';
    if (value % 3600 === 0) return `${value / 3600} 小时`;
    if (value % 60 === 0) return `${value / 60} 分钟`;
    return `${value} 秒`;
  }

  function truncate(value, max) {
    const text = String(value || '');
    return text.length > max ? `${text.slice(0, max - 1)}…` : text;
  }

  function toast(title, message, type = '') {
    const icons = { success: '✓', error: '!', warning: '!', '': 'i' };
    const item = node('div', { className: `toast ${type}`.trim() },
      node('div', { className: 'toast-icon', textContent: icons[type] || 'i' }),
      node('div', { className: 'toast-copy' }, node('strong', { textContent: title }), node('span', { textContent: message })),
      actionButton('×', () => item.remove(), 'toast-close')
    );
    $('toasts').append(item);
    const timeout = setTimeout(() => item.remove(), type === 'error' ? 8000 : 5000);
    item.addEventListener('mouseenter', () => clearTimeout(timeout), { once: true });
  }

  $('login-form').addEventListener('submit', submitCredentialLogin);
  $('login-token-toggle').addEventListener('click', toggleCredentialVisibility);
  document.querySelectorAll('[data-close-dialog]').forEach((button) => button.addEventListener('click', () => closeDialog(button.dataset.closeDialog)));
  document.querySelectorAll('dialog').forEach((dialog) => {
    dialog.addEventListener('click', (event) => { if (event.target === dialog) closeDialog(dialog.id); });
    dialog.addEventListener('cancel', () => { if (dialog.id === 'confirm-dialog') resetConfirmState(); });
    dialog.addEventListener('close', () => {
      if (dialog.id === 'confirm-dialog') resetConfirmState();
      if (state.renderPending && !document.querySelector('dialog[open]')) {
        render();
        state.renderPending = false;
      }
    });
  });
  document.querySelectorAll('[data-project-mode]').forEach((button) => button.addEventListener('click', () => setProjectMode(button.dataset.projectMode)));
  document.querySelectorAll('input[name$="-mode"]').forEach((input) => input.addEventListener('change', syncBindingInputs));
  $('room-project-id').addEventListener('change', () => {
    const project = state.snapshot?.projects?.find((item) => item.id === $('room-project-id').value);
    $('room-dialog-title').textContent = `在 ${projectName(project)} 中创建 Room`;
  });
  $('project-form').addEventListener('submit', submitProject);
  $('room-form').addEventListener('submit', createRoom);
  $('rename-form').addEventListener('submit', submitRename);
  $('binding-form').addEventListener('submit', submitBindingCompletion);
  $('confirm-form').addEventListener('submit', submitConfirm);
  $('confirm-input').addEventListener('input', syncConfirmRequirement);
  $('confirm-ack').addEventListener('change', syncConfirmRequirement);
  $('refresh-button').addEventListener('click', () => refresh({ notify: true, forceRender: true }));
  $('logout-button').addEventListener('click', logoutBrowserSession);
  $('retry-button').addEventListener('click', () => connect({ notify: true, forceRender: true }));
  $('add-project-button').addEventListener('click', () => openProjectDialog('register'));
  $('global-search').addEventListener('input', (event) => {
    state.search = event.target.value;
    if (state.route.name !== 'projects' && state.search) navigate('#/projects');
    else if (state.route.name === 'projects') renderProjects();
  });
  $('global-search').addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      event.currentTarget.value = '';
      state.search = '';
      if (state.route.name === 'projects') renderProjects();
      event.currentTarget.blur();
    }
  });
  $('mobile-menu').addEventListener('click', () => app.classList.add('sidebar-open'));
  $('sidebar-backdrop').addEventListener('click', () => app.classList.remove('sidebar-open'));
  $('sidebar-collapse').addEventListener('click', () => app.classList.toggle('sidebar-collapsed'));
  window.addEventListener('hashchange', () => {
    if (new URLSearchParams(location.hash.replace(/^#/, '')).has('token')) {
      location.reload();
      return;
    }
    state.route = parseRoute();
    app.classList.remove('sidebar-open');
    if (!state.authenticated) return;
    render();
    if (!view.hidden) view.focus({ preventScroll: true });
    window.scrollTo({ top: 0, behavior: 'instant' });
  });
  $('room-picker-button')?.addEventListener('click', openRoomPicker);
  $('room-picker-search')?.addEventListener('input', renderRoomPicker);
  window.addEventListener('message', (event) => {
    if (event.origin !== location.origin) return;
    const data = event.data;
    if (!data || data.type !== 'pairroom-surface') return;
    const frames = Array.from(document.querySelectorAll('#room-stage iframe'));
    if (!frames.some((frame) => frame.contentWindow === event.source)) return;
    if (data.action === 'close-tab') {
      closeTab(data.roomId || state.route.roomID);
      return;
    }
    if (!data.roomId) return;
    const meta = {
      unread: Number(data.unread || 0),
      pendingApprovals: Number(data.pendingApprovals || 0),
      error: data.error || '',
    };
    const previous = state.tabMeta[data.roomId];
    if (previous && previous.unread === meta.unread &&
        previous.pendingApprovals === meta.pendingApprovals && previous.error === meta.error) return;
    state.tabMeta[data.roomId] = meta;
    syncRoomTree();
    syncRoomTabs();
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === '/' && !event.metaKey && !event.ctrlKey && !event.altKey && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement?.tagName)) {
      event.preventDefault();
      $('global-search').focus();
    }
    if (!state.authenticated || event.ctrlKey || event.metaKey || !event.altKey) return;
    if (document.querySelector('dialog[open]')) return;
    const key = event.key;
    if (key === '[' && !event.shiftKey) { event.preventDefault(); shiftTab(-1); }
    else if (key === ']' && !event.shiftKey) { event.preventDefault(); shiftTab(1); }
    else if (key === '{' || (key === '[' && event.shiftKey)) { event.preventDefault(); moveActiveTab(-1); }
    else if (key === '}' || (key === ']' && event.shiftKey)) { event.preventDefault(); moveActiveTab(1); }
    else if (key.toLocaleLowerCase() === 'w') {
      event.preventDefault();
      if (state.route.name === 'room') closeTab(state.route.roomID);
    } else if (key.toLocaleLowerCase() === 'n') {
      event.preventDefault();
      openRoomPicker();
    }
  });
  document.addEventListener('visibilitychange', () => {
    if (state.authenticated && !document.hidden) refresh({ forceRender: state.renderPending });
  });

  function loadStoredTheme() {
    let theme = '';
    try { theme = String(window.localStorage.getItem('pairroom.theme') || ''); } catch { theme = ''; }
    if (theme === 'light' || theme === 'dark') state.preferences.theme = theme;
  }

  function persistTheme(value) {
    try {
      if (value === 'light' || value === 'dark') window.localStorage.setItem('pairroom.theme', value);
      else window.localStorage.removeItem('pairroom.theme');
    } catch { /* theme persistence is best-effort */ }
  }

  loadStoredTheme();
  window.addEventListener('storage', (event) => {
    if (event.key !== 'pairroom.theme') return;
    loadStoredTheme();
    applyPreferences();
  });

  applyPreferences();
  scheduleRefresh();
  renderLoading();
  connect({ forceRender: true });
})();
