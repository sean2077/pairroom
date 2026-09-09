(() => {
  'use strict';

  const t = (key, options) => window.PairRoomI18n ? window.PairRoomI18n.t(key, options) : key;

  const INITIAL_ROUTE = '#/overview';
  const NEW_BINDING_HINT_KEY = 'room.materializesOnFirstTurn';
  const MAX_ROOM_BATCH_SIZE = 100;
  const GITHUB_REPOSITORY_URL = 'https://github.com/sean2077/pairroom';
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
    refreshRevision: 0,
    refreshOptions: {},
    sessionGeneration: 0,
    openingBrowsers: new Set(),
    busyButtons: new WeakSet(),
    refreshTimer: null,
    renderPending: false,
    renderedSnapshotKey: '',
    csrfToken: '',
    tabs: [],
    tabMeta: {},
    activating: new Set(),
    expandedProjects: new Set(),
    knownProjects: new Set(),
    projectFilters: new Map(),
    archivedOpen: new Set(),
    dragTabID: '',
    projectMode: 'register',
    bindingRoomID: '',
    roomDialogRevision: 0,
    pairProfileMode: false,
    pairProfileReady: false,
    pairProfileID: '',
    pairProfileBusy: false,
    agentPairProfiles: null,
    agentPairProfilesPromise: null,
    agentPairProfilesRevision: 0,
    agentPairProfilesError: '',
    contextRoomID: '',
    contextTrigger: null,
    contextScrollPositions: new Map(),
    contextPositionRule: null,
    confirmAction: null,
    confirmRevision: 0,
    confirmRequirement: '',
    confirmAcknowledgementRequired: false,
    selectedRoomIDs: new Set(),
	settingsSection: 'interface',
 desktopStartup: {enabled: null, pending: false, error: ''},
	agentCatalog: null,
	agentCatalogPromise: null,
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
  const diagnostics = window.PairRoomDiagnostics.create({ t, node, actionButton, api, confirm: openConfirm, navigate });

  const ordering = window.PairRoomOrder.create({ t, node, getSnapshot: () => state.snapshot, api, refresh, render, toast,
    isCurrent: () => { const generation = state.sessionGeneration; return () => generation === state.sessionGeneration && state.authenticated; } });

  // Startup translation is declarative; a rendered state/operation takes over
  // its label so later global localization cannot restore misleading defaults.
  function setRenderedText(id, value) {
    const element = $(id);
    element.removeAttribute('data-i18n');
    element.textContent = value;
  }

  async function createBrowserSession(credential = '') {
    const generation = invalidateSessionReads();
    const token = String(credential || '').trim();
    const headers = new Headers();
    const method = token ? 'POST' : 'GET';
    if (token) headers.set('Authorization', `Bearer ${token}`);
    const response = await fetch('/api/v1/session', { method, headers, credentials: 'same-origin' });
    const payload = await response.json().catch(() => ({}));
    if (generation !== state.sessionGeneration) {
      const error = new Error('Obsolete browser session response');
      error.code = 'obsolete_session';
      throw error;
    }
    if (!response.ok) {
      const message = response.status === 401
        ? (token
          ? t("ui.theServiceTokenIsInvalidCheckItAndRetry")
          : t("ui.youAreNotSignedInEnterTheServiceTokenOrRunPairroom"))
		: (window.PairRoomI18n?.errorMessage(payload) || payload.error || response.statusText || `HTTP ${response.status}`);
      const error = new Error(message);
      error.status = response.status;
      if (response.status === 401) error.code = token ? 'invalid_credential' : 'login_required';
      throw error;
    }
    state.csrfToken = payload.csrf_token || '';
    if (!state.csrfToken) throw new Error(t("ui.theManagementSessionDidNotReturnACsrfTokenSignInAgain"));
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
    button.textContent = visible ? t("ui.hide") : t("ui.show");
    button.setAttribute('aria-pressed', String(visible));
    button.setAttribute('aria-label', visible ? t("ui.hideServiceToken") : t("ui.showServiceToken"));
  }

  function toggleCredentialVisibility() {
    setCredentialVisibility($('login-token').type === 'password');
    $('login-token').focus({ preventScroll: true });
  }

  function invalidateSessionReads() {
    diagnostics.reset();
    ordering.cancel();
    state.projectFilters.clear();
    state.sessionGeneration += 1;
    state.refreshPromise = null;
    state.refreshOptions = {};
    state.agentCatalog = null;
    state.agentCatalogPromise = null;
    state.agentPairProfiles = null;
    state.agentPairProfilesPromise = null;
    state.agentPairProfilesRevision += 1;
    state.agentPairProfilesError = '';
    $('refresh-button').classList.remove('spinning');
    return state.sessionGeneration;
  }

  function showCredentialLogin(message = '') {
    closeRoomContextMenu();
    invalidateSessionReads();
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
    scheduleRefresh();
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
    document.title = t("ui.signInPairroom");
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
        ? t("ui.theFullManagementUrlDoesNotContainToken")
        : t("ui.pleaseEnterAServiceTokenOrFullManagementUrl"));
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
        if (error.code === 'obsolete_session') return;
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
        toast(t("ui.signOutFailed"), error.message, 'error');
      }
    });
  }

  async function connect(options = {}) {
    try {
      if (!state.csrfToken) await initializeSession();
      showManagementShell();
      return await refresh(options);
    } catch (error) {
      if (error.code === 'obsolete_session') return null;
      const message = error.code === 'login_required' ? '' : error.message;
      showCredentialLogin(message);
      if (options.notify && message) toast(t("ui.connectionFailed"), message, 'error');
      return null;
    }
  }

  async function api(path, options = {}) {
    const generation = state.sessionGeneration;
    const headers = new Headers(options.headers || {});
    const method = String(options.method || 'GET').toUpperCase();
    if (!['GET', 'HEAD', 'OPTIONS'].includes(method) && state.csrfToken) headers.set('X-PairRoom-CSRF', state.csrfToken);
    if (options.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
    const response = await fetch(path, { ...options, method, headers, credentials: 'same-origin' });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) {
	  const error = new Error(window.PairRoomI18n?.errorMessage(payload) || payload.error || response.statusText || `HTTP ${response.status}`);
      error.status = response.status;
      if (response.status === 401 && path !== '/api/v1/session' && generation === state.sessionGeneration) {
        error.code = 'login_required';
        showCredentialLogin(t("ui.theBrowserSessionHasExpiredPleaseReEnterTheServiceToken"));
      }
      throw error;
    }
    return payload;
  }

  function snapshotRenderKey(snapshot) {
    if (!snapshot) return '';
    // generated_at is request metadata. A busy Runtime or one with in-flight
    // Room HTTP (including SSE) advances last_used_at on every status read,
    // while a truly idle Runtime's activity timestamp is meaningful to the
    // Runtimes table and must still trigger a refresh.
    const renderableSnapshot = { ...snapshot };
    delete renderableSnapshot.generated_at;
    if (Array.isArray(renderableSnapshot.runtimes)) {
      renderableSnapshot.runtimes = renderableSnapshot.runtimes.map((runtime) => {
        const renderableRuntime = { ...runtime };
        if (renderableRuntime.busy || renderableRuntime.http_in_use) delete renderableRuntime.last_used_at;
        return renderableRuntime;
      });
    }
    return JSON.stringify(renderableSnapshot);
  }
  function refresh({ notify = false, forceRender = false, fresh = false } = {}) {
    // A mutation invalidates reads already in flight. Ordinary polls coalesce;
    // callers after writes wait for a read that actually started after them.
    if (fresh) state.refreshRevision += 1;
    state.refreshOptions.notify ||= notify;
    state.refreshOptions.forceRender ||= forceRender;
    if (notify || forceRender) $('refresh-button').classList.add('spinning');
    if (state.refreshPromise) return state.refreshPromise;
    const generation = state.sessionGeneration;
    const request = (async () => {
      while (generation === state.sessionGeneration) {
        const revision = state.refreshRevision;
        let snapshot;
        try {
          snapshot = await api('/api/v1/service');
        } catch (error) {
          if (generation !== state.sessionGeneration) return null;
          if (revision !== state.refreshRevision) continue;
          throw error;
        }
        if (generation !== state.sessionGeneration || !state.authenticated) return null;
        if (revision !== state.refreshRevision) continue;
        const { notify, forceRender } = state.refreshOptions;
        const nextRenderKey = snapshotRenderKey(snapshot);
        const snapshotChanged = nextRenderKey !== state.renderedSnapshotKey;
        state.snapshot = snapshot;
        if (state.contextRoomID && !roomByID(state.contextRoomID)) closeRoomContextMenu();
        pruneRoomSelection(snapshot);
        state.connected = true;
        state.lastError = '';
        updateChrome();
        if (forceRender || snapshotChanged) {
          if (forceRender || canRenderNow()) render();
          else {
            state.renderPending = true;
            window.dispatchEvent(new Event('pairroom:management-render-pending'));
          }
        } else state.renderPending = false;
        if (notify) toast(t("ui.synchronized"), t("ui.managementShellStatusRefreshed"), 'success');
        return snapshot;
      }
      return null;
    })().catch((error) => {
      if (generation !== state.sessionGeneration || error.status === 401) return null;
      const changed = state.connected || state.lastError !== error.message;
      state.connected = false;
      state.lastError = error.message;
      setDisconnected(error.message);
      if (state.refreshOptions.notify || changed) toast(t("ui.connectionFailed"), error.message, 'error');
      return null;
    }).finally(() => {
      if (state.refreshPromise !== request) return;
      state.refreshPromise = null;
      state.refreshOptions = {};
      $('refresh-button').classList.remove('spinning');
      scheduleRefresh();
    });
    state.refreshPromise = request;
    return request;
  }

  function canRenderNow() {
    if (ordering.interacting() || document.querySelector('dialog[open]')) return false;
    const active = document.activeElement;
    if (!active || active === document.body || active === $('global-search')) return true;
    return !view.contains(active) || !['INPUT', 'SELECT', 'TEXTAREA'].includes(active.tagName);
  }

  function scheduleRefresh() {
    if (state.refreshTimer) clearTimeout(state.refreshTimer);
    state.refreshTimer = null;
    if (!state.authenticated || document.hidden) return;
    // Activation is asynchronous (202). Reuse the one refresh timer to observe
    // open tab readiness, even when ordinary auto-refresh is disabled.
    const pending = state.tabs.some(roomID => ['starting', 'queued'].includes(getRuntime(roomID).phase));
    const delay = pending ? 1000 : state.preferences.refreshMs;
    if (delay > 0) {
      state.refreshTimer = setTimeout(() => {
        state.refreshTimer = null;
        return refresh();
      }, delay);
    }
  }

  function parseRoute() {
    const raw = location.hash.startsWith('#/') ? location.hash.slice(2) : 'overview';
    const parts = raw.split('/').filter(Boolean).map((part) => {
      try { return decodeURIComponent(part); } catch { return part; }
    });
    if (parts[0] === 'projects' && parts[1]) return { name: 'project', projectID: parts[1] };
    if (parts[0] === 'diagnostics') {
      history.replaceState(null, '', `#/settings/diagnostics${parts[1] ? '/' + encodeURIComponent(parts[1]) : ''}`);
      return { name: 'settings', section: 'service', roomID: parts[1] || '' };
    }
    if (parts[0] === 'settings') {
      const section = parts[1] === 'diagnostics' ? 'service' : parts[1];
      const known = ['interface', 'pair-profiles', 'runtime', 'operations', 'service', 'boundaries', 'about', ...(window.PairRoomDesktop ? ['desktop'] : [])];
      return { name: 'settings', section: section && (known.includes(section) ? section : 'interface'), roomID: section === 'service' ? parts[2] || '' : '' };
    }
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
    setRenderedText('sidebar-health', state.connected ? (healthy ? t('common.serviceHealthy') : t('common.serviceFailClosed')) : t('common.serviceDisconnected'));
    $('sidebar-version').textContent = snapshot ? `PairRoom ${snapshot.version || ''}`.trim() : 'PairRoom Service';
    $('health-dot').className = `status-dot ${state.connected ? (healthy ? 'good' : 'danger') : 'danger'}`;
    $('nav-project-count').textContent = snapshot ? formatNumber(summary.projects) : '';
    $('nav-runtime-count').textContent = snapshot && summary.runtime_capacity_used ? formatNumber(summary.runtime_capacity_used) : '';
    const routeInfo = routeMetadata();
    setRenderedText('page-title', routeInfo.title);
    setRenderedText('page-subtitle', routeInfo.subtitle);
    document.title = `${routeInfo.title} · PairRoom`;
    document.querySelectorAll('[data-nav]').forEach((node) => {
      const current = state.route.name === 'project' ? 'projects' : (state.route.name === 'room' ? '' : state.route.name);
      node.classList.toggle('active', node.dataset.nav === current);
      if (node.dataset.nav === current) node.setAttribute('aria-current', 'page');
      else node.removeAttribute('aria-current');
    });
    if (snapshot?.generated_at) setRenderedText('last-updated', t("ui.lastSynchronizedValue", { value0: (formatRelativeTime(snapshot.generated_at)) }));
    syncRoomTree();
    syncRoomTabs();
  }

  function setDisconnected(message) {
    $('connection-banner').hidden = false;
    setRenderedText('connection-message', message || t("ui.waitingForTheLocalServiceToRecover"));
    setRenderedText('sidebar-health', t('common.serviceDisconnected'));
    $('health-dot').className = 'status-dot danger';
  }

  function routeMetadata() {
    const snapshot = state.snapshot;
    switch (state.route.name) {
      case 'projects':
        return { title: t('ui.projectsAndRooms'), subtitle: t("ui.manageCanonicalGitWorktreesAndCollaborationRoomsThatAreIsolatedFromEach") };
      case 'project': {
        const project = snapshot?.projects?.find((item) => item.id === state.route.projectID);
        return { title: projectName(project) || t('common.project'), subtitle: project?.root || t("ui.checkTheProjectIdentityRoomAndRunningStatus") };
      }
      case 'runtimes':
        return { title: t('room.roomRuntimes'), subtitle: t("ui.viewCapacityQueuesActiveTurnAndIdlePendingStatus") };
      case 'settings':
        return { title: t("ui.settings"), subtitle: t("ui.adjustTheCurrentAdminPageExperienceAndCheckServiceStartupPoliciesAnd") };
      case 'room': {
        const room = roomByID(state.route.roomID);
        const runtime = getRuntime(state.route.roomID);
        return {
          title: room?.name || t('common.room'),
          subtitle: runtimeLabel(runtime),
        };
      }
      default:
        return { title: t("ui.overview"), subtitle: t("ui.multiProjectLocalCollaborationControlSurfaceForSupportedRuntimes") };
    }
  }

  function render() {
    if (ordering.interacting()) { state.renderPending = true; return; }
    state.route = parseRoute();
    if (state.route.name === 'settings' && state.route.section) state.settingsSection = state.route.section;
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
      if (!room || room.lifecycle === 'archived') {
        toast(t(room ? 'ui.roomArchived' : 'room.noLongerAvailable'), room ? t('ui.canOnlyBeOpenedAfterRecovery') : '', 'warning');
        closeTab(state.route.roomID);
        return;
      }
      const runtime = getRuntime(state.route.roomID);
      if (room && !roomHasBlockingPendingBindings(room) && runtime.phase === 'suspended') {
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
    if (ordering.interacting()) return;
    const tree = $('room-tree');
    if (!tree) return;
    const snapshot = state.snapshot;
    if (!snapshot) {
      tree.replaceChildren();
      return;
    }
    const models = buildProjectModels(snapshot);
    const activeRoomID = state.route.name === 'room' ? state.route.roomID : '';
    // Polling may rebuild the tree while a keyboard user is navigating it.
    // Restore stable control identity without scrolling the sidebar sideways.
    const focused = tree.contains(document.activeElement) ? document.activeElement : null;
    const focusAttribute = ['aria-controls', 'href', 'data-room-id'].find((attribute) => focused?.hasAttribute(attribute));
    const focusSelector = focusAttribute ? `[${focusAttribute}="${CSS.escape(focused.getAttribute(focusAttribute))}"]` : '';
    const scrollTop = tree.scrollTop;
    tree.replaceChildren(...models.map((model) => {
      const { project, rooms } = model;
      if (!state.knownProjects.has(project.id)) {
        state.knownProjects.add(project.id);
        state.expandedProjects.add(project.id);
      }
      const expanded = state.expandedProjects.has(project.id);
      const currentProject = state.route.name === 'project' && state.route.projectID === project.id;
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
            textContent: showArchived ? t("ui.hideArchivedValue", { value0: (archivedRooms.length) }) : t("ui.archivedValue", { value0: (archivedRooms.length) }),
            onClick: () => {
              if (showArchived) state.archivedOpen.delete(project.id);
              else state.archivedOpen.add(project.id);
              syncRoomTree();
            },
          }));
          if (showArchived) archivedRooms.forEach((room) => children.push(renderTreeRoom(room, false)));
        }
      }
      const roomsID = `project-rooms-${encodeURIComponent(project.id)}`;
      return node('section', { className: `tree-project ${expanded ? 'open' : ''}` },
        ordering.decorate(node('div', { className: `tree-project-header ${currentProject ? 'current' : ''}` },
          node('button', {
            type: 'button', className: 'tree-project-toggle',
            'aria-label': t(expanded ? 'workspace.collapse' : 'workspace.expand', { name: projectName(project) }),
            'aria-expanded': String(expanded), 'aria-controls': roomsID,
            onClick: () => {
              if (expanded) state.expandedProjects.delete(project.id);
              else state.expandedProjects.add(project.id);
              syncRoomTree();
              tree.querySelector(`[aria-controls="${CSS.escape(roomsID)}"]`)?.focus({ preventScroll: true });
            },
          }, node('span', { className: 'tree-caret', 'aria-hidden': 'true', textContent: expanded ? '▾' : '▸' })),
          node('a', {
            className: 'tree-project-link', href: `#/projects/${encodeURIComponent(project.id)}`,
            title: project.root, 'aria-current': currentProject ? 'page' : null,
          }, node('strong', { textContent: projectName(project) }), node('span', { className: 'nav-count', textContent: formatNumber(activeRooms.length) }))
        ), 'project', project.id),
        node('div', { id: roomsID, className: 'tree-rooms', hidden: !expanded }, ...children)
      );
    }));
    if (focusSelector) tree.querySelector(focusSelector)?.focus({ preventScroll: true });
    tree.scrollTop = scrollTop;
  }

  function renderTreeRoom(room, current) {
    const runtime = getRuntime(room.id);
    const archived = room.lifecycle === 'archived';
    const meta = state.tabMeta[room.id] || {};
    const badges = [];
    if (meta.unread) badges.push(node('span', { className: 'tab-badge', textContent: formatNumber(meta.unread) }));
    if (meta.pendingApprovals) badges.push(node('span', { className: 'tab-badge warn', textContent: '!' }));
    if (meta.error) badges.push(node('span', { className: 'tab-badge danger', textContent: '×' }));
    const button = node('button', {
      type: 'button',
      className: `tree-room ${current ? 'active' : ''} ${archived ? 'archived' : ''}`,
      'data-room-id': room.id,
      'aria-haspopup': 'menu',
      title: archived ? t("ui.archivedCanOnlyBeOpenedAfterRestoration") : room.name,
      onClick: () => {
        if (archived) {
          toast(t("ui.roomArchived"), t("ui.canOnlyBeOpenedAfterRecovery"), 'warning');
          return;
        }
        openRoom(room.id);
      },
    }, node('span', { className: `tree-room-dot ${runtimeTone(runtime)}`, 'aria-hidden': 'true' }), node('span', { className: 'tree-room-name', textContent: room.name }), ...badges);
    return ordering.decorate(node('div', { className: 'tree-room-row' }, button), 'room', room.id);
  }

  function syncRoomTabs() {
    const list = $('room-tablist');
    if (!list) return;
    // A different client may archive/remove a background Room. Prune both its
    // tab and its surface from the same authoritative snapshot.
    if (state.snapshot) {
      const available = new Set((state.snapshot.rooms || []).filter((room) => room.lifecycle !== 'archived').map((room) => room.id));
      state.tabs = state.tabs.filter((id) => {
        if (available.has(id)) return true;
        delete state.tabMeta[id];
        return false;
      });
    }
    $('room-tabstrip').hidden = state.tabs.length === 0;
    const keep = new Set(state.tabs);
    Array.from(list.children).forEach((tab) => { if (!keep.has(tab.dataset.roomId)) tab.remove(); });
    state.tabs.forEach((roomID, index) => {
      const room = roomByID(roomID);
      const meta = state.tabMeta[roomID] || {};
      const selected = state.route.name === 'room' && state.route.roomID === roomID;
      let tab = list.querySelector(`[data-room-id="${CSS.escape(roomID)}"]`);
      if (!tab) {
        tab = node('div', {
          className: 'room-tab', role: 'tab', draggable: 'true', 'data-room-id': roomID,
          onClick: () => openRoom(roomID),
          onDragStart: (event) => { state.dragTabID = roomID; event.dataTransfer.setData('text/plain', roomID); },
          onDragOver: (event) => event.preventDefault(),
          onDrop: (event) => {
            event.preventDefault();
            reorderTab(state.dragTabID || event.dataTransfer.getData('text/plain'), state.tabs.indexOf(roomID));
          },
        }, node('span', { className: 'tree-room-dot', 'aria-hidden': 'true' }),
        node('span', { className: 'room-tab-label' }), node('span', { className: 'tab-badge' }),
        node('button', { type: 'button', className: 'room-tab-close', textContent: '×',
          onClick: (event) => { event.stopPropagation(); closeTab(roomID); } }));
      }
      // Keep enhanced tab targets and focused controls attached during polls
      // and unread-count updates. management-ux owns keyboard enhancements.
      tab.classList.toggle('active', selected);
      const target = tab.querySelector('.room-tab-target') || tab;
      target.setAttribute('aria-selected', String(selected));
      target.tabIndex = selected || (state.route.name !== 'room' && index === 0) ? 0 : -1;
      const dot = tab.querySelector('.tree-room-dot');
      const dotClass = `tree-room-dot ${runtimeTone(getRuntime(roomID))}`;
      if (dot.className !== dotClass) dot.className = dotClass;
      const label = tab.querySelector('.room-tab-label');
      if (label.textContent !== (room?.name || roomID)) label.textContent = room?.name || roomID;
      const badge = tab.querySelector('.tab-badge');
      badge.hidden = !meta.unread;
      const unread = meta.unread ? formatNumber(meta.unread) : '';
      if (badge.textContent !== unread) badge.textContent = unread;
      tab.querySelector('.room-tab-close').setAttribute('aria-label', t('ui.closeValue', { value0: room?.name || roomID }));
      if (list.children[index] !== tab) list.insertBefore(tab, list.children[index] || null);
    });
    syncRoomStage();
    window.dispatchEvent(new Event('pairroom:tabs-updated'));
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
    const title = phase === 'queued' ? t("ui.queuedValue", { value0: (runtime.queue_position || '?') }) : (phase === 'starting' ? t("ui.startingRuntime") : (phase === 'failed' ? t("ui.runtimeFailed") : t("ui.runtimeHasHung")));
    const detail = runtime.last_error || t("ui.switchingBackToThisTabWillAutomaticallyReRequestActivationTheBackground");
    return node('div', { className: 'room-placeholder' },
      node('p', { className: 'eyebrow', textContent: t('common.roomSurfaceUpper') }),
      node('h2', { textContent: room?.name || t('common.room') }),
      node('p', { textContent: `${title} · ${detail}` }),
      actionButton(t("ui.reactivate"), () => activateRoomRuntime(room.id), 'primary-button')
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
    if (index < 0) {
      if (state.route.name === 'room' && state.route.roomID === roomID) navigate('#/overview');
      return;
    }
    state.tabs.splice(index, 1);
    scheduleRefresh();
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
      scheduleRefresh();
      return runtime;
    }
    state.activating.add(roomID);
    try {
      const status = await api(`/api/v1/rooms/${encodeURIComponent(roomID)}/activate`, { method: 'POST' });
      await refresh({ forceRender: true, fresh: true });
      return status;
    } catch (error) {
      toast(t("ui.roomActivationFailed"), error.message, 'error');
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
    }) : [node('p', { className: 'muted', textContent: t("ui.thereIsNoMatchingActiveRoom") })]));
  }

  function renderLoading() {
    view.replaceChildren(
      node('div', { className: 'view-stack' },
        node('div', { className: 'loading-grid' },
          ...Array.from({ length: 4 }, () => node('div', { className: 'panel skeleton loading-card' }))
        ),
        node('div', { className: 'panel skeleton skeleton-tall' })
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
      ? t("ui.valueActiveRoomsAcrossValueProjects", { value0: (summary.active_rooms), value1: (summary.projects) })
      : t("ui.startTheControlPlaneWithYourFirstGitProject");
    const heroText = projects.length
      ? t("ui.eachRoomHasExclusiveNativeSessionsSwitchingManagementViews")
      : t("ui.explicitlyRegisterTheCanonicalGitWorktreeAndThenCreateIsolatedAgentRooms");

    const heroActions = [
      actionButton(t("ui.registerProject"), () => openProjectDialog('register'), 'primary-button'),
    ];
    if (activeProjects.length) heroActions.push(actionButton(t("ui.createRoom"), () => openRoomDialog(activeProjects[0].id), 'secondary-button'));

    view.replaceChildren(
      node('div', { className: 'view-stack' },
        node('section', { className: 'panel hero-panel' },
          node('div', { className: 'hero-copy' },
            node('p', { className: 'eyebrow', textContent: t('common.localPairingControlPlaneUpper') }),
            node('h2', { textContent: heroTitle }),
            node('p', { textContent: heroText }),
            node('div', { className: 'hero-actions' }, ...heroActions)
          ),
          node('div', { className: 'hero-meta' },
            heroMeta(t("ui.runtimeCapacity"), policy.limit ? `${formatNumber(summary.runtime_capacity_used)} / ${formatNumber(policy.limit)}` : formatNumber(summary.runtime_capacity_used)),
            heroMeta(t("ui.idleSuspend"), policy.idle_timeout_seconds ? formatDuration(policy.idle_timeout_seconds) : t("ui.determinedByStartupParameters")),
            heroMeta(t("ui.operatingMode"), snapshot.healthy ? t('common.failSafe') : t('common.failClosed')),
            heroMeta(t("ui.version"), snapshot.version || t('common.development'))
          )
        ),
        node('section', { className: 'stats-grid', 'aria-label': t("ui.serviceStatistics") },
          statCard(t('common.projects'), summary.projects, t("ui.valueUnavailable", { value0: (summary.unavailable_projects) }), '⌂', 'accent', () => navigate('#/projects')),
          statCard(t("ui.activityRoom"), summary.active_rooms, t("ui.valueArchived", { value0: (summary.archived_rooms) }), '◇', 'good', () => navigate('#/projects')),
          statCard(t("ui.working"), summary.busy_runtimes, t("ui.valueQueued", { value0: (summary.queued_runtimes) }), '◎', summary.queued_runtimes ? 'warn' : '', () => navigate('#/runtimes')),
          statCard(t("ui.needAttention"), summary.attention_items, summary.attention_items ? t("ui.bindingRuntimePathOrCleanupDiagnostics") : t("ui.thereAreCurrentlyNoBlocks"), '!', summary.attention_items ? 'danger' : 'good')
        ),
        node('section', { className: 'two-panel-grid' },
          panel(t("ui.needAttention"), t("ui.blockActivationOrProjectsThatRequireManualProcessing"),
            attention.length ? node('div', { className: 'list' }, ...attention.slice(0, 8).map(renderAttentionItem))
              : emptyState('✓', t("ui.everythingIsFine"), t("ui.thereAreCurrentlyNoUnavailableProjectsFailedRuntimesBindingsToBeCompleted"), true),
            attention.length > 8 ? t("ui.valueItemsHidden", { value0: (attention.length - 8) }) : ''
          ),
          panel(t("ui.runInRealTime"), t("ui.activeWorkingAndQueuedRooms"),
            live.length ? node('div', { className: 'list' }, ...live.slice(0, 8).map(renderLiveItem))
              : emptyState('◎', t("ui.thereIsCurrentlyNoActiveRuntime"), t("ui.afterOpeningARoomTheRuntimeWillStartLazilyBasedOnCapacity"), true)
          )
        ),
        panel(t('common.projects'), t("ui.overviewOfRecentlyRegisteredWorkspacesAndRooms"),
          projects.length ? node('div', { className: 'list' }, ...projects.slice(0, 6).map(renderProjectOverviewItem))
            : emptyState('⌂', t("ui.notYetRegisteredProject"), t("ui.onlyAbsolutePathsEnteredExplicitlyByTheUserAreAcceptedAndDevelopment"), true, actionButton(t("ui.registerYourFirstProject"), () => openProjectDialog('register'), 'primary-button compact-button')),
          projects.length > 6 ? t("ui.viewAllWorkspacesOnTheProjectsPage") : '',
          projects.length > 6 ? actionButton(t("ui.viewAll"), () => navigate('#/projects'), 'text-button') : null
        ),
        node('aside', { className: 'callout boundary' },
          node('strong', { textContent: t('common.transcriptBoundary') }),
          node('span', { textContent: t("ui.reusingExistingSessionThreadOnlyRestoresTheVendorContextThePublicTimeline") })
        )
      )
    );
  }

  function renderProjects() {
    const snapshot = state.snapshot;
    const projectModels = buildProjectModels(snapshot)
      .filter((model) => projectMatchesFilters(model));

    const availability = selectControl([
      ['all', t("ui.allStatus")], ['available', t('common.available')], ['unavailable', t('common.unavailable')],
    ], state.filters.projectAvailability, (value) => { state.filters.projectAvailability = value; renderProjects(); }, t("ui.projectStatus"));

    view.replaceChildren(
      node('div', { className: 'view-stack' },
        node('section', { className: 'section-header' },
          node('div', {}, node('h2', { textContent: t("ui.workspaceManagement") }), node('p', { textContent: t("ui.aProjectCorrespondsToACanonicalGitWorktreeEachProjectCanHave") })),
          node('div', { className: 'section-actions' },
            actionButton(t("ui.importLegacyRoom"), () => openProjectDialog('import'), 'secondary-button'),
            actionButton(t("ui.registerProject"), () => openProjectDialog('register'), 'primary-button')
          )
        ),
        node('section', { className: 'panel panel-body toolbar' },
          node('label', { className: 'search-field' },
            node('span', { textContent: '⌕', 'aria-hidden': 'true' }),
            node('input', {
              type: 'search', value: state.search, placeholder: t("ui.filterByPathProjectRoomOrId"),
              'aria-label': t("ui.filterProjectAndRoom"),
              onInput: (event) => { state.search = event.target.value; $('global-search').value = state.search; renderProjects(); },
            })
          ),
          availability,
          node('span', { className: 'muted', textContent: t('room.projectFilterCount', { visible: projectModels.length, total: (snapshot.projects || []).length }) })
        ),
        snapshot.navigation_order_error ? node('aside', { className: 'callout warning', role: 'alert', textContent: t('workspace.ordering.unavailable') }) : null,
        projectModels.length
          ? node('section', { className: 'project-grid' }, ...projectModels.map(renderProjectCard))
          : emptyState('⌕', t("ui.noMatchingProject"), state.search ? t("ui.adjustYourSearchTermsOrFilters") : t("ui.registerAGitWorktreeToGetStarted"), false,
            state.search ? actionButton(t("ui.clearFilters"), () => { state.search = ''; $('global-search').value = ''; state.filters.projectAvailability = 'all'; renderProjects(); }, 'secondary-button')
              : actionButton(t("ui.registerProject9c99cf3"), () => openProjectDialog('register'), 'primary-button')),
        node('aside', { className: 'callout neutral' },
          node('strong', { textContent: t('room.projectIdentity') }),
          node('span', { textContent: t("ui.theServiceResolvesSymbolicLinksAndPerformsGitWorktreeRootNormalizationEquivalent") })
        )
      )
    );
  }

  function renderProjectCard(model) {
    const { project, rooms, activeRooms, runtimeCounts } = model;
    const openProject = `#/projects/${encodeURIComponent(project.id)}`;
    const heading = node('div', { className: 'project-list-identity' },
      node('div', { className: 'project-avatar', textContent: projectInitials(project), 'aria-hidden': 'true' }),
      node('div', { className: 'project-card-title' },
        node('h2', {}, node('a', { href: openProject, textContent: projectName(project) })),
        node('code', { className: 'project-path', textContent: project.root, title: project.root }))
    );
    // Whole-row navigation mirrors the room-tab container click. The row takes no
    // role and no tabindex: the Project-name link stays the single keyboard and
    // screen-reader entry point, while the ordering gesture keeps owning drag,
    // right-click, and Alt+Arrow. Nested controls own their own clicks, and
    // returning before navigate() leaves native link behaviour — middle click,
    // modifier click — intact.
    const row = ordering.decorate(node('article', {
      className: 'panel project-card project-list-row',
      onClick: (event) => {
        if (event.target instanceof Element && event.target.closest('a, button, input, select, textarea, label, summary')) return;
        navigate(openProject);
      },
    },
      heading,
      node('div', { className: 'project-list-status' },
        statusBadge(project.available ? 'available' : 'unavailable', project.available ? 'good' : 'danger'),
        node('span', { textContent: t('room.projectWorkingSummary', { rooms: activeRooms, working: runtimeCounts.busy }) }),
        rooms.length > activeRooms ? node('span', { className: 'muted', textContent: t('ui.valueArchivedc521841', { value0: rooms.length - activeRooms }) }) : null),
      node('div', { className: 'project-list-actions' },
        !project.available && state.snapshot?.capabilities?.project_refresh ? actionButton(t('ui.recheck'), (event) => { event.stopPropagation(); refreshProject(project); }, 'secondary-button compact-button') : null,
        actionButton(t('room.addRoom'), (event) => { event.stopPropagation(); openRoomDialog(project.id); }, 'primary-button compact-button', !project.available))
    ), 'project', project.id);
    // Only a sortable row was given a drag tooltip, so only a sortable row should
    // advertise drag controls alongside the new click-to-open affordance.
    if (row.title) row.title = t('workspace.ordering.helpProjectRow');
    return row;
  }

  function renderProjectDetail(projectID) {
    const snapshot = state.snapshot;
    const model = buildProjectModels(snapshot).find((item) => item.project.id === projectID);
    if (!model) {
      view.replaceChildren(emptyState('?', t("ui.projectDoesNotExist"), t("ui.itMayNotBeRegisteredYetOrTheServiceRegistryMayHave"), false, actionButton(t("ui.returnToProjects"), () => navigate('#/projects'), 'secondary-button')));
      return;
    }
    const { project, rooms, activeRooms, runtimeCounts, runtimeByRoom } = model;
    let filter = state.projectFilters.get(projectID);
    if (!filter) { filter = { search: '', showArchived: false, phase: 'all' }; state.projectFilters.set(projectID, filter); }
    const visibleRooms = rooms.filter((room) => (filter.showArchived || room.lifecycle !== 'archived')
      && (!filter.search || `${room.name} ${room.id}`.toLocaleLowerCase().includes(filter.search.toLocaleLowerCase()))
      && (filter.phase === 'all' || (filter.phase === 'attention' ? (runtimeByRoom.get(room.id)?.phase === 'failed' || roomHasBlockingPendingBindings(room)) : runtimeByRoom.get(room.id)?.busy)));
    // Provider display names come from the Agent catalog, which the Room dialog
    // otherwise loads on demand. A direct navigation to a Project has not loaded
    // it, so request it once and re-render only while this Project is still the
    // visible route. An unavailable catalog is not an error here: the rows keep
    // showing the compact reference.
    if (!state.agentCatalog && !state.agentCatalogPromise && visibleRooms.some((room) => room.agents)) {
      loadAgentCatalog().then((catalog) => {
        // Any Project view benefits from the names, not only the one that started
        // the request. Honour the same focus/dialog deferral as the refresh path
        // so an in-flight catalog cannot destroy a caret mid-typing.
        if (catalog && state.route.name === 'project') {
          if (canRenderNow()) render();
          else state.renderPending = true;
        }
      }).catch(() => {});
    }
    const search = node('input', { id: 'project-room-search', type: 'search', value: filter.search, placeholder: t('workspace.searchRooms'), 'aria-label': t('workspace.searchRooms'), onInput: (event) => {
      filter.search = event.target.value;
      const cursor = event.target.selectionStart;
      renderProjectDetail(projectID);
      $('project-room-search').focus({ preventScroll: true });
      $('project-room-search').setSelectionRange(cursor, cursor);
    } });
    const phase = node('select', { id: 'project-room-filter', 'aria-label': t('workspace.filterRooms'), onChange: (event) => { filter.phase = event.target.value; renderProjectDetail(projectID); $('project-room-filter').focus({ preventScroll: true }); } },
      ...['all', 'attention', 'working'].map((key) => node('option', { value: key, textContent: t(`workspace.filter.${key}`) })));
    phase.value = filter.phase;
    view.replaceChildren(
      node('div', { className: 'view-stack project-workspace' },
        actionButton(t("ui.backToProjects"), () => navigate('#/projects'), 'text-button'),
        node('section', { className: 'panel detail-hero' },
          node('div', { className: 'detail-title' },
            node('div', { className: 'project-avatar', textContent: projectInitials(project) }),
            node('div', {},
              node('div', { className: 'room-title-line' }, node('h2', { className: 'flush-heading', textContent: projectName(project) }), statusBadge(project.available ? 'available' : 'unavailable', project.available ? 'good' : 'danger')),
              node('p', { className: 'detail-path', textContent: project.root })
            )
          ),
          node('div', { className: 'detail-actions' },
            actionButton(t('diagnostics.title'), () => navigate('#/settings/diagnostics'), 'secondary-button'),
            actionButton(t("ui.copyPath"), () => copyText(project.root, t("ui.projectPathCopied")), 'secondary-button'),
            actionButton(t("ui.createRoom"), () => openRoomDialog(project.id), 'primary-button', !project.available)
          )
        ),
        snapshot.navigation_order_error ? node('aside', { className: 'callout warning', role: 'alert', textContent: t('workspace.ordering.unavailable') }) : null,
        project.diagnostic ? node('aside', { className: 'callout danger' }, node('strong', { textContent: t("ui.projectIsNotAvailable") }), node('span', { textContent: project.diagnostic })) : null,
        node('section', { className: 'stats-grid' },
          statCard(t("workspace.activeRooms"), activeRooms, t("ui.valueArchivedc521841", { value0: (rooms.length - activeRooms) }), '◇', 'accent'),
          statCard(t("workspace.working"), runtimeCounts.busy, t('room.activeCount', { count: runtimeCounts.active }), '◎', runtimeCounts.busy ? 'warn' : ''),
          statCard(t("workspace.queued"), runtimeCounts.queued, runtimeCounts.queued ? t("ui.waitingForGlobalCapacity") : t("ui.thereIsCurrentlyNoWaiting"), '↥', runtimeCounts.queued ? 'warn' : 'good'),
          statCard(t("workspace.failed"), runtimeCounts.failed, runtimeCounts.failed ? t("ui.viewRuntimeDiagnostics") : t("ui.noFailureRuntime"), '!', runtimeCounts.failed ? 'danger' : 'good')
        ),
        node('section', { className: 'project-room-section' },
          panel(t('common.rooms'), t("ui.publicTimelinesAttachmentsApprovalsAndAgentBindingsAreAllIsolatedByRoom"),
            node('div', {},
              node('div', { className: 'project-room-tools' }, search, phase, node('span', { className: 'muted', role: 'status', textContent: t('workspace.results', { count: visibleRooms.length }) })),
              visibleRooms.length ? node('div', { className: 'room-list' }, ...visibleRooms.map((room) => renderRoomRow(room, runtimeByRoom.get(room.id))))
              : emptyState('◇', t(filter.search || filter.phase !== 'all' ? 'workspace.noMatches' : 'ui.noVisibleRoom'), t('workspace.emptyHelp'), true, actionButton(t('workspace.clearFilters'), () => { filter.search = ''; filter.phase = 'all'; renderProjectDetail(projectID); }, 'secondary-button'))
            ),
            '',
            node('div', { className: 'section-actions' },
              actionButton(filter.showArchived ? t("ui.hideArchived") : t("ui.showArchived9738720"), () => { filter.showArchived = !filter.showArchived; renderProjectDetail(projectID); }, 'secondary-button compact-button'),
              state.snapshot?.capabilities?.room_deletion ? roomSelectionToggleButton(visibleRooms, t("ui.theCurrentVisibleRangeOfThisProject")) : null,
              state.snapshot?.capabilities?.room_deletion ? roomClearSelectionButton() : null,
              state.snapshot?.capabilities?.room_deletion && selectedActiveRooms(visibleRooms).length ? roomBatchArchiveButton(selectedActiveRooms(visibleRooms)) : null,
              state.snapshot?.capabilities?.room_deletion && selectedArchivedRooms(visibleRooms).length ? roomBatchRemovalButton(selectedArchivedRooms(visibleRooms)) : null
            )
          )
        ),
        node('details', { className: 'project-details panel' },
          node('summary', { textContent: t('workspace.projectDetails') }),
          panel(t('room.projectIdentity'), t("ui.canonicalWorktreeRecordsInTheRegistry"),
            node('div', { className: 'key-value-grid' },
              keyValue(t('room.projectId'), project.id, true),
              keyValue(t("ui.creationTime"), formatDateTime(project.created_at)),
              keyValue(t('room.canonicalRoot'), project.root, true),
              keyValue(t("ui.availability"), project.available ? t('common.available') : t('common.unavailable'))
            ),
            project.diagnostic || t("ui.serviceDoesNotImplicitlySwitchProjectFromTheCurrentWorkingDirectory")
          ),
        state.snapshot?.capabilities?.project_refresh || state.snapshot?.capabilities?.project_removal
          ? panel(t('room.projectMaintenance'), t("ui.recheckTheCanonicalPathOrSafelyLogOutOfTheEmptyProject"),
            node('div', { className: 'section-actions' },
              state.snapshot?.capabilities?.project_refresh
                ? actionButton(t("ui.recheckPath"), () => refreshProject(project), 'secondary-button')
                : null,
              projectRemovalButton(project, rooms.length)
            ),
            rooms.length
              ? t("ui.stillContainsValueRoomsIncludingArchivedRoomsArchiveAndPermanentlyDeleteEvery", { value0: (rooms.length) })
              : t("ui.unregisteringRemovesOnlyTheRegistryEntryItDoesNotDeleteTheGit")
          )
          : null
        ),
        node('aside', { className: 'callout boundary' }, node('strong', { textContent: t('room.workspaceBoundary') }), node('span', { textContent: t("ui.roomPermanentlyBelongsToThisProjectReviewerSnapshotAndGitStatusAre") }))
      )
    );
  }

  function renderRoomRow(room, runtime = suspendedRuntime(room.id)) {
    const pending = roomHasBlockingPendingBindings(room);
    const archived = room.lifecycle === 'archived';
    const title = node('div', { className: 'room-title-line' },
      node('strong', { textContent: room.name }),
      archived ? statusBadge('archived', 'warn') : null,
	  !archived ? statusBadge(runtimeLabel(runtime), runtimeTone(runtime), runtime.busy ? 'busy' : '') : null,
	  room.legacy ? statusBadge('legacy', 'info') : null,
	  room.legacy_defaults ? statusBadge(t('room.legacyDefaults'), 'info') : null
	);
    // Three aligned groups: the two durable Agent slots and the Room identity.
    // The native session name is not rendered here because it repeats the Room
    // name, the current mention handle, and the short Room ID — all already
    // visible in this row — so it survives only as the group tooltip.
    const meta = node('div', { className: 'room-meta' },
      roomAgentGroup('claude', room),
      roomAgentGroup('codex', room),
      node('div', { className: 'room-meta-group room-id-group' },
        node('span', { className: 'room-meta-label', textContent: t('room.roomId') }),
        node('code', { className: 'room-meta-value', textContent: room.id, title: room.id })),
    );
    if (runtime.last_error) meta.append(node('span', { className: 'badge danger plain room-meta-error', textContent: truncate(runtime.last_error, 90), title: runtime.last_error }));

    const actions = node('div', { className: 'room-actions' });
    if (runtime.phase === 'failed' && room.agents?.claude && room.agents?.codex) actions.append(actionButton(t('diagnostics.title'), () => navigate(`#/settings/diagnostics/${encodeURIComponent(room.id)}`), 'secondary-button compact-button room-action-control'));
    if (state.snapshot?.capabilities?.room_deletion) {
      actions.append(
        node('label', {
          className: 'button secondary-button compact-button room-action-control room-select-control',
          title: archived ? t("ui.selectThisRoomForBatchCleaning") : t("ui.selectThisRoomForBatchArchiving"),
        },
          node('input', {
            type: 'checkbox',
            checked: state.selectedRoomIDs.has(room.id),
            'aria-label': t("ui.selectRoomValue", { value0: (room.name) }),
            onChange: (event) => toggleRoomSelection(room.id, event.target.checked),
          }),
          node('span', { textContent: t("ui.choose") })
        )
      );
    }
    if (archived) {
      actions.append(actionButton(t("ui.restore"), () => restoreRoom(room), 'secondary-button compact-button room-action-control'));
      if (state.snapshot?.capabilities?.room_deletion) {
        actions.append(actionButton(t("ui.permanentlyDelete"), () => confirmRoomRemoval([room]), 'danger-button outline compact-button room-action-control'));
      }
    } else {
      if (pending) actions.append(actionButton(t("ui.completeBindings"), () => completeBindings(room), 'primary-button compact-button room-action-control'));
      actions.append(actionButton(runtime.phase === 'queued' ? t("ui.queuedValue", { value0: (runtime.queue_position || '?') }) : t("ui.open"), () => openRoom(room.id), 'primary-button compact-button room-action-control', pending));
      actions.append(actionButton(t("ui.browserOpens"), () => openRoomInBrowserAction(room.id), 'secondary-button compact-button room-action-control', pending));
      actions.append(actionButton(t("ui.rename"), () => openRenameDialog(room), 'secondary-button compact-button room-action-control'));
      actions.append(actionButton(t("ui.archive"), () => archiveRoom(room), 'danger-button outline compact-button room-action-control'));
    }
    return ordering.decorate(node('article', { className: 'room-row', 'data-room-id': room.id }, node('div', { className: 'room-row-main' }, title, meta), actions), 'room', room.id);
  }

  // One aligned column per durable Agent slot: runtime display name, binding
  // state, model, and Provider display name. A legacy Room has no immutable
  // selection and says so instead of implying one.
  function roomAgentGroup(actor, room) {
    const selection = room.agents?.[actor];
    const binding = room.bindings?.[actor];
    const runtimeName = room.runtime_names?.[actor] || '';
    const title = [
      runtimeName,
      runtimeName ? t('room.runtimeNameOnActivation') : '',
      binding?.session_id || bindingText(binding),
    ].filter(Boolean).join('\n');
    return node('div', { className: 'room-meta-group', 'data-slot': actor, title },
      node('span', { className: 'room-meta-label', textContent: actor === 'claude' ? t('agent.agent1') : t('agent.agent2') }),
      node('div', { className: 'room-meta-lines' },
        node('span', { className: 'room-meta-line', textContent: selection ? runtimeDisplayName(selection.runtime) : t('room.legacyDefaults') }),
        node('span', { className: `badge plain binding-chip ${bindingTone(binding)}`.trim(), textContent: bindingText(binding) }),
        selection ? node('span', { className: 'room-meta-line', textContent: selection.model || t('room.nativeDefault'), title: t('agent.model') }) : null,
        selection ? node('span', { className: 'room-meta-line', textContent: providerDisplayName(selection.provider), title: providerTooltip(selection.provider) }) : null,
      ));
  }

  function bindingTone(binding) {
    if (!binding) return '';
    if (binding.pending) return binding.mode === 'new' ? 'info' : 'warn';
    return 'good';
  }

  function runtimeDisplayName(runtime) {
    return runtimeCatalogEntry(runtime)?.display_name || runtime || '';
  }

  // Join the immutable reference against the Agent catalog exactly the way the
  // creation form does. A Profile removed from CC Switch stays visible as a
  // compact reference rather than silently falling back to native.
  function providerDisplayName(ref) {
    if (ref?.source !== 'cc-switch') return t('agent.nativeProvider');
    const match = (state.agentCatalog?.profiles || []).find((entry) => providerOptionValue(entry.provider) === providerOptionValue(ref));
    return match?.name || `${ref.app_type}/${ref.profile_id}`;
  }

  function providerTooltip(ref) {
    const described = ref?.source === 'cc-switch' ? `CC Switch · ${ref.app_type}/${ref.profile_id}` : t('agent.nativeProvider');
    return `${t('agent.provider')}: ${described}. ${t('room.agentConfigurationImmutable')}`;
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
      ['all', t("ui.allRuntime")], ['active', t('common.active')], ['queued', t('ui.queued')], ['starting', t('common.starting')], ['stopping', t('common.stopping')], ['suspended', t('common.suspended')], ['failed', t('ui.failed')],
    ], state.filters.runtimePhase, (value) => { state.filters.runtimePhase = value; renderRuntimes(); }, t("ui.runtimePhase"));

    view.replaceChildren(
      node('div', { className: 'view-stack' },
        node('section', { className: 'section-header' },
          node('div', {}, node('h2', { textContent: t('room.runtimeOrchestration') }), node('p', { textContent: t("ui.onlyIdleRuntimeIsRecycledActiveTurnsAreNeverPreemptedDueTo") })),
          node('div', { className: 'section-actions' }, phaseSelect, actionButton(t("ui.refreshStatus"), () => refresh({ notify: true, forceRender: true }), 'secondary-button'))
        ),
        node('section', { className: 'three-panel-grid' },
          node('article', { className: 'panel capacity-card' },
            node('div', { className: 'capacity-heading' }, node('div', {}, node('div', { className: 'stat-label', textContent: t("ui.globalRuntimeCapacity") }), node('div', { className: 'capacity-value' }, formatNumber(summary.runtime_capacity_used), node('small', { textContent: ` / ${policy.limit ? formatNumber(policy.limit) : '—'}` }))), statusBadge(percent >= 100 ? 'full' : 'available', percent >= 100 ? 'warn' : 'good')),
            node('progress', { className: 'progress-track', max: String(limit), value: String(summary.runtime_capacity_used), 'aria-label': t("ui.globalRuntimeCapacity") }),
            node('div', { className: 'capacity-legend' }, node('span', {}, node('i'), t('room.activeCount', { count: summary.active_runtimes })), node('span', {}, node('i', { className: 'busy' }), t('room.workingCount', { count: summary.busy_runtimes })), node('span', {}, node('i', { className: 'queued' }), t('room.queuedCount', { count: summary.queued_runtimes })))
          ),
          statCard(t('room.idleTimeout'), policy.idle_timeout_seconds ? formatDuration(policy.idle_timeout_seconds) : '—', t("ui.countingFromLastActivity"), '◷', 'accent'),
          statCard(t('ui.queue'), summary.queued_runtimes, summary.queued_runtimes ? t("ui.fifoDisconnectingTheBrowserDoesNotCancelTheDemand") : t("ui.thereIsCurrentlyNoWaiting"), '↥', summary.queued_runtimes ? 'warn' : 'good')
        ),
        queued.length ? panel(t('room.activationQueue'), t("ui.whenAllCapacityIsOccupiedByTheWorkingRuntimeTheNewRoom"),
          node('div', { className: 'list' }, ...queued.map((item) => renderQueueItem(item)))
        ) : null,
        panel(t("ui.allRoomRuntime"), t("ui.valueRoomsSortedByWorkPriority", { value0: (models.length) }),
          models.length ? runtimeTable(models) : emptyState('◎', t("ui.noMatchingRuntime"), t("ui.adjustStatusFilter"), true),
          t("ui.failedAndStillOccupyingCapacityMeansThatTheCleanupStatusIsUncertain")
        ),
        node('aside', { className: 'callout warning' },
          node('strong', { textContent: t('room.nonPreemptivePolicy') }),
          node('span', { textContent: t("ui.suspendOnlyTakesEffectForIdleQueuedOrRuntimesThatCanBe") })
        )
      )
    );
  }

  function runtimeTable(models) {
    const table = node('table', { className: 'runtime-table' });
    table.append(node('thead', {}, node('tr', {}, ...[t('common.room'), t('common.project'), t("ui.stage"), t("ui.capacity"), t("ui.lastActivity"), t("ui.actions")].map((label) => node('th', { textContent: label })))));
    const body = node('tbody');
    models.forEach(({ room, project, runtime }) => {
      const actionCell = node('div', { className: 'runtime-actions' });
      const cleanupUncertain = runtime.phase === 'failed' && runtime.occupies_capacity;
      if (room.agents?.claude && room.agents?.codex) actionCell.append(actionButton(t('diagnostics.title'), () => navigate(`#/settings/diagnostics/${encodeURIComponent(room.id)}`), 'secondary-button compact-button'));
      if (room.lifecycle !== 'archived') {
        actionCell.append(actionButton(
          cleanupUncertain ? t("ui.requiresControlledRestart") : (runtime.phase === 'active' ? t("ui.open") : t("ui.activate")),
          () => openRoom(room.id),
          'secondary-button compact-button',
          roomHasBlockingPendingBindings(room) || cleanupUncertain,
        ));
      }
      if (['active', 'queued', 'starting'].includes(runtime.phase)) {
        actionCell.append(actionButton(runtime.phase === 'queued' ? t("ui.cancelQueue") : t("ui.suspend"), () => suspendRoom(room, runtime), 'danger-button outline compact-button', runtime.busy || runtime.phase === 'starting'));
      }
      body.append(node('tr', { className: 'runtime-record' },
        node('td', { 'data-label': t('common.room') }, node('div', { className: 'runtime-room' }, node('strong', { textContent: room.name }), node('small', { textContent: room.id }))),
        node('td', { 'data-label': t('common.project'), textContent: projectName(project) || t('common.unknown') }),
        node('td', { 'data-label': t("ui.stage") }, statusBadge(runtimeLabel(runtime), runtimeTone(runtime), runtime.busy ? 'busy' : '')),
        node('td', { 'data-label': t("ui.capacity"), textContent: runtime.occupies_capacity ? t("ui.occupy") : '—' }),
        node('td', { 'data-label': t("ui.lastActivity"), textContent: runtime.last_used_at ? formatRelativeTime(runtime.last_used_at) : '—', title: runtime.last_used_at ? formatDateTime(runtime.last_used_at) : '' }),
		node('td', { 'data-label': t("ui.actions"), className: 'runtime-action-cell' }, actionCell)
      ));
      if (runtime.last_error) {
        body.append(node('tr', { className: 'runtime-error-row' }, node('td', { colspan: '6', className: 'runtime-error-cell' }, node('div', { className: 'callout danger' }, node('strong', { textContent: t('room.runtimeError') }), node('span', { textContent: runtime.last_error })))));
      }
    });
    table.append(body);
    return node('div', { className: 'panel-body flush' }, table);
  }

  function renderQueueItem({ room, project, runtime }) {
    return node('article', { className: 'list-item' },
      node('div', { className: 'list-main' }, node('div', { className: 'item-symbol accent', textContent: `#${runtime.queue_position || '?'}` }), node('div', { className: 'list-copy' }, node('strong', { textContent: room.name }), node('p', { textContent: `${projectName(project)} · ${t('room.queuedAtValue', { value: runtime.queued_at ? formatRelativeTime(runtime.queued_at) : '—' })}` }))),
      node('div', { className: 'list-meta' }, actionButton(t("ui.cancelQueue"), () => suspendRoom(room, runtime), 'secondary-button compact-button'))
    );
  }

  function renderSettings() {
    if (state.settingsSection === 'pair-profiles' && !state.agentPairProfiles && !state.agentPairProfilesPromise && !state.agentPairProfilesError) refreshPairProfileSettings();
    const sections = [
      ['interface', t("ui.interfaceExperience")], ['pair-profiles', t('agent.pairProfile.title')], ['runtime', t("ui.runtimeStrategy")], ['operations', t("ui.daemonOperationAndMaintenance")], ['service', t('diagnostics.title')], ['boundaries', t("ui.securityBoundary")], ['about', t("ui.about")],
    ];
    if (window.PairRoomDesktop) sections.splice(1, 0, ['desktop', t('desktop.settings')]);
    const nav = node('nav', { className: 'panel settings-nav', 'aria-label': t("ui.setUpPartitions") }, ...sections.map(([key, label]) => {
      const active = state.settingsSection === key;
      const button = actionButton(label, () => { if (key === 'pair-profiles') refreshPairProfileSettings(); if (key === 'desktop') updateDesktopStartup(); navigate(`#/settings/${key === 'service' ? 'diagnostics' : key}`); }, active ? 'active' : '');
      button.dataset.settingsSection = key;
      if (active) button.setAttribute('aria-current', 'page');
      button.setAttribute('aria-pressed', String(active));
      return button;
    }));
    const content = node('section', { className: 'settings-content' }, renderSettingsSection());
    view.replaceChildren(
      node('div', { className: 'view-stack' },
        node('section', { className: 'section-header' },
          node('div', {}, node('h2', { textContent: t('room.managementSettings') }), node('p', { textContent: t("ui.interfacePreferencesOnlyApplyToTheCurrentTabServicePoliciesAreDetermined") }))
        ),
        node('div', { className: 'settings-grid' }, nav, content)
      )
    );
    if (state.settingsSection === 'service') diagnostics.render($('settings-diagnostics'), state.snapshot, state.route.roomID || '');
  }

  function renderSettingsSection() {
    const snapshot = state.snapshot;
    const policy = runtimePolicy(snapshot);
    if (state.settingsSection === 'pair-profiles') return renderPairProfileSettings();
    if (state.settingsSection === 'desktop' && window.PairRoomDesktop) return renderDesktopSettings();
    if (state.settingsSection === 'runtime') {
      const command = runtimeCommand(policy);
      return node('div', { className: 'view-stack' },
        settingsPanel(t("ui.effectiveRuntimePolicies"), t("ui.theNonPreemptibleSchedulingParametersActuallyUsedByTheCurrentProcess"),
          settingRow(t("ui.maxActivityRuntime"), t("ui.startingActiveStoppingAndCleaningUpUncertainFailedRuntimeAllOccupyCapacity"), runtimeLimitControl(policy)),
          settingRow(t('room.idleTimeout'), t("ui.theCalculationOnlyStartsWhenThereIsNoActiveTurnInThe"), node('strong', { textContent: policy.idle_timeout_seconds ? formatDuration(policy.idle_timeout_seconds) : t("ui.notExposed") })),
          settingRow(t('room.reconcileInterval'), t("ui.howOftenTheRuntimeManagerChecksIdleQueueAndCapacity"), node('strong', { textContent: policy.poll_interval_milliseconds ? `${window.PairRoomI18n.formatNumber(policy.poll_interval_milliseconds)} ms` : t("ui.notExposed") })),
          settingRow(t('room.closeTimeout'), t("ui.theSingleDeadlineForSafelyShuttingDownTheRoomRuntime"), node('strong', { textContent: policy.close_timeout_seconds ? formatDuration(policy.close_timeout_seconds) : t("ui.notExposed") }))
        ),
        node('section', { className: 'panel' },
          node('header', { className: 'panel-header' }, node('div', { className: 'panel-header-copy' }, node('h2', { textContent: t("ui.adjustStartupParameters") }), node('p', { textContent: t("ui.policyChangesRequireAControlledRestartToAvoidDynamicReconfigurationOfRunning") }))),
          node('div', { className: 'panel-body' },
            node('div', { className: 'command-box' }, node('code', { textContent: command }), actionButton(t("ui.copy"), () => copyText(command, t("ui.theStartupCommandHasBeenCopied")), 'secondary-button compact-button'))
          ),
          node('footer', { className: 'panel-footer', textContent: t("ui.thisFrontEndExampleOnlyCoversTheRuntimeParametersPleaseRetainOther") })
        ),
        node('aside', { className: 'callout warning' }, node('strong', { textContent: t("ui.capacityAndIdle") }), node('span', { textContent: t("ui.capacityCanBeAdjustedImmediatelyOnThisPageLoweringTheLimitWill") }))
      );
    }
    if (state.settingsSection === 'operations') {
      const daemonInstall = daemonInstallCommand(policy);
      return node('div', { className: 'view-stack' },
        settingsPanel(t("ui.daemonShortcutCommand"), t("ui.pairroomWebShellDoesNotDirectlyStopOrRestartTheHostProcess"),
          settingRow(t("ui.openManagementShell"), t("ui.parseAndVerifyTheCompleteAuthenticationAddressOfTheCurrentDaemonAnd"), inlineCommand('pairroom daemon open', t("ui.daemonOpenCommandCopied"))),
          settingRow(t("ui.checkStatus"), t("ui.displaysInstallationStatusPlatformPidLogsAndRotationMetadata"), inlineCommand('pairroom daemon status', t("ui.daemonStatusCommandHasBeenCopied"))),
          settingRow(t("ui.followTheLog"), t("ui.readTheMergedStdoutStderrLogManagedByTheDaemon"), inlineCommand('pairroom daemon logs -f', t("ui.daemonLogsCommandHasBeenCopied"))),
          settingRow(t("ui.controlledRestart"), t("ui.inheritTheCompleteInstalledServiceDefinitionAndWaitForTheActiveTurn"), inlineCommand('pairroom daemon restart', t("ui.theDaemonRestartCommandHasBeenCopied")))
        ),
        node('section', { className: 'panel' },
          node('header', { className: 'panel-header' }, node('div', { className: 'panel-header-copy' }, node('h2', { textContent: t("ui.updateInstalledRuntimeParameters") }), node('p', { textContent: t("ui.daemonRestartDoesNotAcceptNewServiceParametersTheCompleteInstallationDefinition") }))),
          node('div', { className: 'panel-body command-stack' },
            node('div', { className: 'command-example' },
              node('div', { className: 'command-example-heading' }, node('strong', { textContent: t("ui.defaultParameterExample") }), node('span', { textContent: t("ui.verifyExistingDaemonDefinitionsBeforeCopying") })),
              node('div', { className: 'command-box' }, node('code', { textContent: daemonInstall }), actionButton(t("ui.copyExample"), () => copyText(daemonInstall, t("ui.daemonInstallExampleCopied")), 'secondary-button compact-button'))
            )
          ),
          node('footer', { className: 'panel-footer', textContent: t("ui.forceReplacesTheServiceDefinitionIfYouAreCurrentlyUsingCustomConfig") })
        ),
        node('aside', { className: 'callout boundary' }, node('strong', { textContent: t('room.daemonBoundary') }), node('span', { textContent: t("ui.theDaemonOnlyProjectsThePairroomServiceToSystemdLaunchdOrWindows") })),
        node('aside', { className: 'callout warning' }, node('strong', { textContent: t('room.crashStaleServiceLock') }), node('span', { textContent: t("ui.onlyAfterConfirmingThatTheOldProcessHasDisappearedCanYouExplicitly") }))
      );
    }
    if (state.settingsSection === 'about') {
      return node('div', { className: 'view-stack' },
        node('section', { className: 'panel' },
          node('header', { className: 'panel-header' }, node('div', { className: 'panel-header-copy' }, node('h2', { textContent: t('ui.about') }), node('p', { textContent: t('ui.buildAndRepositoryIdentity') }))),
          node('div', { className: 'key-value-grid' },
            keyValue(t('ui.version'), snapshot.version || t('common.development')),
            keyValue(t('room.commit'), snapshot.commit || t('room.notEmbedded'), true),
            keyValue(t('room.buildDate'), snapshot.build_date || t('room.notEmbedded')),
            keyValue(t('ui.storeSchema'), snapshot.store_schema ? String(snapshot.store_schema) : t('room.notEmbedded')),
            keyValue(t('room.dataRoot'), snapshot.data_root, true),
            keyValue(t('ui.githubRepository'), githubRepositoryLink(snapshot.repository_url)),
            keyValue(t('ui.license'), t('ui.mitLicense'))
          )
        )
      );
    }
    if (state.settingsSection === 'service') {
      const safe = diagnosticSnapshot();
      return node('div', { className: 'view-stack' },
        node('div', { id: 'settings-diagnostics' }),
        node('details', { className: 'project-details panel service-support' },
          node('summary', { textContent: t('workspace.ordering.serviceSummary') }),
        node('section', { className: 'panel' },
          node('header', { className: 'panel-header' }, node('div', { className: 'panel-header-copy' }, node('h2', { textContent: t('room.serviceIdentity') }), node('p', { textContent: t("ui.buildInformationAndStableDataRoots") }))),
          node('div', { className: 'key-value-grid' },
            keyValue(t('room.generatedAt'), formatDateTime(snapshot.generated_at)),
            keyValue(t('room.registryHealth'), snapshot.healthy ? t('common.healthy') : t('common.failClosed'))
          )
        ),
        settingsPanel(t("ui.diagnosticTools"), t("ui.exportingContentRemovesTheRoomRuntimeUrlLocalPathsAndBusinessMetadata"),
          settingRow(t("ui.copyServiceSummary"), t("ui.suitableForPastingIntoALocalIssueOrDebuggingSession"), actionButton(t("ui.copyJson"), () => copyText(JSON.stringify(safe, null, 2), t("ui.desensitizationDiagnosticsHaveBeenReproduced")), 'secondary-button')),
          settingRow(t("ui.downloadDiagnosticFiles"), t("ui.filesAreOnlyGeneratedLocallyInTheBrowserAndAreNotUploaded"), actionButton(t("ui.downloadJson"), downloadDiagnosticSnapshot, 'secondary-button')),
          settingRow(t("ui.viewOriginalStructure"), t("ui.expandTheDesensitizedServiceSnapshotOnThePage"), toggleButton(state.showRawSnapshot, (value) => { state.showRawSnapshot = value; renderSettings(); }, t("ui.switchSnapshotDisplay")))
        ),
        state.showRawSnapshot ? node('pre', { className: 'raw-snapshot', textContent: JSON.stringify(safe, null, 2) }) : null),
        snapshot.maintenance?.pending_cleanup || snapshot.maintenance?.diagnostic
          ? settingsPanel(t("ui.roomDeleteCleanup"), t("ui.tombstoneCommittedManagedDataInQuarantineStillNeedsToBePhysicallyCleaned"),
            settingRow(
              t("ui.valueCleanupItems", { value0: (snapshot.maintenance?.pending_cleanup || 0) }),
              snapshot.maintenance?.diagnostic || t("ui.itSSafeToTryAgainDeletedRoomsWillNotBeRestored"),
              actionButton(t("ui.retryCleanup"), retryRoomDeletionCleanup, 'secondary-button')
            )
          )
          : null,
        snapshot.diagnostic ? node('aside', { className: 'callout danger' }, node('strong', { textContent: t('room.registryDiagnostic') }), node('span', { textContent: snapshot.diagnostic })) : null
      );
    }
    if (state.settingsSection === 'boundaries') {
      const caps = snapshot.capabilities || {};
      return node('div', { className: 'view-stack' },
        settingsPanel(t('room.controlPlaneCapabilities'), t("ui.theInterfaceOnlyPresentsControlCapabilitiesThatAreExplicitlySupportedByThe"),
          capabilityRow(t("ui.registerCanonicalProject"), true, t("ui.explicitAbsolutePathServerSideDirectoryBrowsingIsNotProvided")),
          capabilityRow(t("ui.importLegacyRoom"), caps.legacy_import !== false, t("ui.nonDestructiveRegistrationDoesNotMoveOrRewriteEventsJsonl")),
          capabilityRow(t("ui.manuallySuspendIdleRuntime"), caps.runtime_suspend === true, t("ui.busyRuntimeWillRejectTheOperation")),
          capabilityRow(t("ui.hotUpdateRuntimeCapacity"), caps.runtime_policy_mutation === true, t("ui.theMaximumNumberOfSimultaneousActiveRoomRuntimesCanBeAdjustedIn")),
          capabilityRow(t("ui.inAppRoomSurface"), caps.room_surface === true, t("ui.theManagementSameOriginGatewayCarriesInApplicationTagsAndDoesNot")),
          capabilityRow(t("ui.roomLifeCycleManagement"), caps.room_deletion === true, t("ui.supportsBatchArchivingAndBatchPermanentCleaningOfUpTo100Rooms")),
          capabilityRow(t("ui.serverPathBrowser"), caps.server_path_browser === true, t("ui.avoidExpandingNativeFileSystemExposure"))
        ),
        node('aside', { className: 'callout boundary' }, node('strong', { textContent: t('common.transcriptBoundary') }), node('span', { textContent: t("ui.vendorTranscriptAndPairroomRoomEventLogAreDifferentRecordsExistingBinding") })),
        node('aside', { className: 'callout warning' }, node('strong', { textContent: t('room.bindingIdentity') }), node('span', { textContent: t("ui.agentVendorSessionIdExclusiveWithinTheEntireServiceIncludingArchivedRooms") })),
        node('aside', { className: 'callout neutral' }, node('strong', { textContent: t('room.browserSession') }), node('span', { textContent: t("ui.theManagementTokenIsOnlyUsedForOneTimeBootstrapAndIs") }))
      );
    }
    return node('div', { className: 'view-stack' },
      settingsPanel(t("ui.appearance"), t("ui.themeIsSharedAndPersistedWithTheEmbeddedRoomOtherPreferencesOnly"),
        settingRow(t("ui.theme"), t("ui.followTheSystemOrTemporarilyFixLightDarkColors"), segmented([
          ['system', t("ui.followTheSystem")], ['light', t("ui.lightColor")], ['dark', t("ui.dark")],
        ], state.preferences.theme, (value) => { state.preferences.theme = value; persistTheme(value); applyPreferences(); renderSettings(); }, t("ui.theme"))),
        settingRow(t("ui.informationDensity"), t("ui.compactShrinksListTableAndPanelSpacing"), segmented([
          ['comfortable', t("ui.comfortable")], ['compact', t("ui.compact")],
        ], state.preferences.density, (value) => { state.preferences.density = value; applyPreferences(); renderSettings(); }, t("ui.informationDensity")))
      ),
      settingsPanel(t("ui.refreshAndNavigation"), t("ui.controlsHowTheCurrentPagePollsTheServiceSidebarClickToAlways"),
        settingRow(t("ui.autoRefresh"), t("ui.automaticallyPausesWhenThePageIsHiddenAndSyncsImmediatelyWhenIt"), selectControl([
          ['0', t("ui.off")], ['5000', t("ui.5Seconds")], ['10000', t("ui.10Seconds")], ['30000', t("ui.30Seconds")], ['60000', t("ui.60Seconds")],
        ], String(state.preferences.refreshMs), (value) => { state.preferences.refreshMs = Number(value); scheduleRefresh(); }, t("ui.autoRefreshInterval"))),
        settingRow(t("ui.archivedByDefault"), t("ui.affectsProjectsAndProjectDetailsLists"), toggleButton(state.filters.showArchived, (value) => { state.filters.showArchived = value; }, t("ui.toggleArchiveVisibility")))
      ),
      node('aside', { className: 'callout neutral' }, node('strong', { textContent: t("ui.noImplicitPersistence") }), node('span', { textContent: t("ui.theseInterfaceOptionsDoNotWriteToTheServiceRegistryAndDo") }))
    );
  }

  async function updateDesktopStartup(enabled) {
    const setting = state.desktopStartup;
    if (!window.PairRoomDesktop || setting.pending) return;
    setting.pending = true;
    setting.error = '';
    try {
      setting.enabled = await (typeof enabled === 'boolean'
        ? window.PairRoomDesktop.setStartup(enabled) : window.PairRoomDesktop.readStartup());
    } catch (error) {
      setting.enabled = typeof error.enabled === 'boolean' ? error.enabled : null;
      setting.error = error.message;
    } finally {
      setting.pending = false;
      if (state.route.name === 'settings' && state.settingsSection === 'desktop') renderSettings();
    }
  }

  function renderDesktopSettings() {
    const setting = state.desktopStartup;
    const toggle = toggleButton(setting.enabled === true, updateDesktopStartup, t('desktop.launchAtLogin'));
    toggle.disabled = setting.pending || setting.enabled === null;
    const rows = [settingRow(t('desktop.launchAtLogin'), t('desktop.launchAtLoginHelp'), toggle)];
    if (setting.pending) rows.push(node('p', {role: 'status', textContent: t('desktop.updating')}));
    if (setting.error) rows.push(node('div', {className: 'callout danger', role: 'alert'},
      node('span', {textContent: setting.error}),
      actionButton(t('ui.retryNow'), () => { updateDesktopStartup(); renderSettings(); }, 'secondary-button')));
    return settingsPanel(t('desktop.settings'), t('desktop.systemSetting'), ...rows);
  }

  function settingsPanel(title, subtitle, ...rows) {
    return node('section', { className: 'panel' },
      node('header', { className: 'panel-header' }, node('div', { className: 'panel-header-copy' }, node('h2', { textContent: title }), node('p', { textContent: subtitle }))),
      node('div', { className: 'setting-list' }, ...rows)
    );
  }

  function runtimeLimitControl(policy) {
    const mutable = state.snapshot?.capabilities?.runtime_policy_mutation === true;
    if (!mutable) return node('strong', { textContent: policy.limit ? String(policy.limit) : t("ui.notExposed") });
    const input = node('input', {
      type: 'number', min: '1', max: '128', value: String(policy.limit || 8),
      'aria-label': t("ui.maximumSimultaneousActivityRoomRuntime"),
      onChange: async (event) => {
        const limit = Number(event.target.value);
        try {
          await api('/api/v1/runtime-policy', { method: 'PATCH', body: JSON.stringify({ limit }) });
          toast(t("ui.runtimeCapacityUpdated"), t("ui.atMostValueRuntimesCanBeActiveSimultaneously", { value0: (limit) }), 'success');
          await refresh({ forceRender: true, fresh: true });
        } catch (error) {
          toast(t("ui.couldNotUpdateCapacity"), error.message, 'error');
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
      actionButton(t("ui.copy"), () => copyText(command, successMessage), 'secondary-button compact-button')
    );
  }

  function attentionItems(snapshot) {
    const items = [];
    (snapshot.projects || []).forEach((project) => {
      if (!project.available) items.push({ kind: 'project', title: projectName(project), detail: project.diagnostic || t("ui.projectRootIsInaccessible"), symbol: '!', tone: 'danger', action: () => navigate(`#/projects/${encodeURIComponent(project.id)}`) });
    });
    (snapshot.rooms || []).forEach((room) => {
      if (roomHasBlockingPendingBindings(room)) {
        const project = projectForRoom(snapshot, room);
        items.push({ kind: 'binding', title: room.name, detail: t("ui.valueLegacyBindingIncomplete", { value0: (projectName(project)) }), symbol: 'B', tone: 'warn', action: () => completeBindings(room) });
      }
    });
    runtimeModels(snapshot).forEach(({ room, runtime }) => {
      if (runtime.phase === 'failed') items.push({ kind: 'runtime', title: room.name, detail: runtime.last_error || t("ui.runtimeFailedToStartOrShutDown"), symbol: 'R', tone: 'danger', action: () => navigate('#/runtimes') });
    });
    const maintenance = snapshot.maintenance || {};
    if (maintenance.pending_cleanup || maintenance.diagnostic) {
      const count = Number(maintenance.pending_cleanup || 0);
      items.push({
        kind: 'maintenance',
        title: t("ui.roomDataCleaningToBeCompleted"),
        detail: maintenance.diagnostic || t("ui.valueQuarantinedCleanupItemsCanBeRetriedSafely", { value0: (count) }),
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
      actionButton(t("ui.dealWith"), item.action, 'secondary-button compact-button')
    );
  }

  function liveRuntimeItems(snapshot) {
    return runtimeModels(snapshot).filter((item) => ['active', 'queued', 'starting', 'stopping'].includes(item.runtime.phase)).sort(compareRuntimeModels);
  }

  function renderLiveItem({ room, project, runtime }) {
    return node('article', { className: 'list-item' },
      node('div', { className: 'list-main' }, node('div', { className: `item-symbol ${runtime.busy ? 'warn' : 'accent'}`, textContent: runtime.busy ? '●' : '◎' }), node('div', { className: 'list-copy' }, node('strong', { textContent: room.name }), node('p', { textContent: projectName(project) }))),
      node('div', { className: 'list-meta' }, statusBadge(runtimeLabel(runtime), runtimeTone(runtime), runtime.busy ? 'busy' : ''), actionButton(t("ui.open"), () => openRoom(room.id), 'secondary-button compact-button', roomHasBlockingPendingBindings(room)))
    );
  }

  function renderProjectOverviewItem(project) {
    const rooms = (state.snapshot.rooms || []).filter((room) => room.project_id === project.id);
    const active = rooms.filter((room) => room.lifecycle !== 'archived').length;
    const busy = rooms.filter((room) => getRuntime(room.id).busy).length;
    return node('article', { className: 'list-item' },
      node('div', { className: 'list-main' }, node('div', { className: 'item-symbol accent', textContent: projectInitials(project) }), node('div', { className: 'list-copy' }, node('strong', { textContent: projectName(project) }), node('p', { textContent: project.root, title: project.root }))),
      node('div', { className: 'list-meta' }, statusBadge(project.available ? 'available' : 'unavailable', project.available ? 'good' : 'danger'), node('span', { className: 'muted', textContent: t('room.projectWorkingSummary', { rooms: active, working: busy }) }), actionButton(t("ui.check"), () => navigate(`#/projects/${encodeURIComponent(project.id)}`), 'secondary-button compact-button'))
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
    return ordering.ordered(snapshot.projects || [], snapshot.navigation_order?.projects).map((project) => {
      const rooms = ordering.ordered(roomsByProject.get(project.id) || [], snapshot.navigation_order?.rooms?.[project.id]).sort((a, b) => Number(a.lifecycle === 'archived') - Number(b.lifecycle === 'archived'));
      const runtimes = rooms.filter((room) => room.lifecycle !== 'archived').map((room) => runtimeByRoom.get(room.id) || suspendedRuntime(room.id));
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
    if (!binding) return t('common.missing');
    if (binding.pending && binding.mode === 'new') return `${t('common.new')} · ${t(NEW_BINDING_HINT_KEY)}`;
    if (binding.pending) return t('room.pendingLegacyBinding');
    const id = String(binding.session_id || '');
    const compact = id.length > 24 ? `${id.slice(0, 10)}…${id.slice(-8)}` : id;
	const mode = binding.mode === 'new' ? t('common.new') : binding.mode === 'existing' ? t('common.existing') : (binding.mode || t('common.existing'));
    return `${mode}${compact ? ` · ${compact}` : ''}`;
  }

  function runtimeLabel(runtime) {
    if (runtime.phase === 'queued') return t('room.queuedPosition', { value: runtime.queue_position || '?' });
    if (runtime.phase === 'active' && runtime.busy) return `${t('common.active')} · ${t('ui.working694b71b')}`;
    return localizedStatus(runtime.phase || 'suspended');
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
    setRenderedText('project-dialog-title', importing ? t("ui.importLegacyRoom") : t("ui.registerProject9c99cf3"));
    setRenderedText('project-dialog-subtitle', importing ? t("ui.explicitlyRegisterCustomOldDataDir") : t("ui.addACanonicalGitWorktree"));
    $('project-path').placeholder = importing ? '/absolute/path/to/legacy/room-data' : '/absolute/path/to/git/worktree';
    setRenderedText('project-submit', importing ? t("ui.importLegacyRoom") : t("ui.registerProject9c99cf3"));
    const help = $('project-mode-help');
    help.replaceChildren(
      node('strong', { textContent: importing ? t("ui.nonDestructiveImport") : t("ui.explicitPathBoundary") }),
      node('span', { textContent: importing
        ? t("ui.eventsJsonlWillNotBeMovedCopiedOrRewrittenItWillAlso")
        : t("ui.onlyAbsolutePathsAreAcceptedRootPathsSubdirectoriesAndSymlinksResolveTo") })
    );
  }

  async function submitProject(event) {
    event.preventDefault();
    const path = $('project-path').value.trim();
    if (!looksAbsolutePath(path)) {
      showFormError('project-form-error', t("ui.pleaseEnterAnAbsolutePathServiceDoesNotAcceptRelativePaths"));
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
        toast(state.projectMode === 'import' ? t("ui.legacyRoomHasBeenImported") : t("ui.projectRegistered"), state.projectMode === 'import' ? t("ui.theOldDataRemainsInPlaceAndHasNotBeenOverwritten") : t("ui.canonicalWorktreeHasJoinedTheServiceRegistry"), 'success');
        await refresh({ forceRender: true, fresh: true });
        const projectID = state.projectMode === 'import' ? result.project_id : result.id;
        if (projectID) navigate(`#/projects/${encodeURIComponent(projectID)}`);
      } catch (error) {
        showFormError('project-form-error', error.message);
      }
    });
  }

  async function loadAgentCatalog(force = false) {
    if (!force && state.agentCatalogPromise) return state.agentCatalogPromise;
    if (!force && state.agentCatalog) return state.agentCatalog;
    const generation = state.sessionGeneration;
    const path = force ? '/api/v1/agent-catalog/refresh' : '/api/v1/agent-catalog';
    const request = api(path, force ? { method: 'POST' } : {}).then(async (catalog) => {
      if (generation !== state.sessionGeneration) {
        const error = new Error('Obsolete Agent catalog response');
        error.code = 'obsolete_session';
        throw error;
      }
      if (state.agentCatalogPromise !== request) return state.agentCatalogPromise || state.agentCatalog;
      state.agentCatalog = catalog;
      return catalog;
    }).finally(() => {
      if (state.agentCatalogPromise === request) state.agentCatalogPromise = null;
    });
    state.agentCatalogPromise = request;
    return request;
  }


  const PAIR_PROFILES_PATH = '/api/v1/agent-pair-profiles';

  async function loadAgentPairProfiles(force = false) {
    if (!force && state.agentPairProfilesPromise) return state.agentPairProfilesPromise;
    if (!force && state.agentPairProfiles) return state.agentPairProfiles;
    const generation = state.sessionGeneration;
    const revision = ++state.agentPairProfilesRevision;
    const request = api(PAIR_PROFILES_PATH).then((catalog) => {
      if (generation !== state.sessionGeneration) {
        const error = new Error('Obsolete Agent pair profiles response');
        error.code = 'obsolete_session';
        throw error;
      }
      if (revision !== state.agentPairProfilesRevision) return state.agentPairProfilesPromise || state.agentPairProfiles;
      if (catalog.schema !== 1 || !Array.isArray(catalog.profiles)) throw new Error(t('agent.pairProfile.loadFailed'));
      state.agentPairProfiles = catalog;
      state.agentPairProfilesError = '';
      return catalog;
    }).finally(() => {
      if (state.agentPairProfilesPromise === request) state.agentPairProfilesPromise = null;
    });
    state.agentPairProfilesPromise = request;
    return request;
  }

  async function mutateAgentPairProfiles(path, options) {
    if (state.pairProfileBusy) throw new Error(t('agent.pairProfile.busy'));
    state.pairProfileBusy = true;
    const generation = state.sessionGeneration;
    // Invalidate in-flight reads, including reads from before this write.
    state.agentPairProfilesRevision += 1;
    state.agentPairProfilesPromise = null;
    try {
      const catalog = await api(PAIR_PROFILES_PATH + path, options);
      if (generation !== state.sessionGeneration) {
        const error = new Error('Obsolete Agent pair profiles response');
        error.code = 'obsolete_session';
        throw error;
      }
      state.agentPairProfilesRevision += 1;
      state.agentPairProfilesPromise = null;
      state.agentPairProfiles = catalog;
      state.agentPairProfilesError = '';
      return catalog;
    } finally {
      state.pairProfileBusy = false;
    }
  }

  async function refreshPairProfileSettings() {
    const generation = state.sessionGeneration;
    try { await loadAgentPairProfiles(true); }
    catch (error) {
      if (generation !== state.sessionGeneration) return;
      state.agentPairProfilesError = error.message;
    }
    if (state.authenticated && state.route.name === 'settings' && state.settingsSection === 'pair-profiles') renderSettings();
  }

  function renderPairProfileSettings() {
    const catalog = state.agentPairProfiles;
    const rows = (catalog?.profiles || []).map((profile) => {
      const isDefault = profile.id === catalog.default_profile_id;
      const summary = ['claude', 'codex'].map((actor) => {
        const agent = profile.agents[actor];
        return [agent.runtime, agent.model || t('common.inherit'), agent.effort].filter(Boolean).join(' · ');
      }).join(' / ');
      const controls = node('div', { className: 'pair-profile-actions' },
        actionButton(t('agent.pairProfile.edit'), () => openRoomDialog('', profile.id), 'secondary-button compact-button'),
        actionButton(isDefault ? t('agent.pairProfile.clearDefault') : t('agent.pairProfile.setDefault'), async () => {
          try {
            await mutateAgentPairProfiles('/default', { method: 'PATCH', body: JSON.stringify({ profile_id: isDefault ? '' : profile.id }) });
            if (state.route.name === 'settings') renderSettings();
          } catch (error) { toast(t('ui.actionFailed'), error.message, 'error'); }
        }, 'secondary-button compact-button'),
        actionButton(t('agent.pairProfile.delete'), () => openConfirm({
          title: t('agent.pairProfile.delete'), message: t('agent.pairProfile.deleteConfirm', { name: profile.name }),
          label: t('agent.pairProfile.delete'), action: async () => {
            await mutateAgentPairProfiles('/' + encodeURIComponent(profile.id), { method: 'DELETE' });
            if (state.route.name === 'settings') renderSettings();
          },
        }), 'secondary-button compact-button'),
      );
      const row = settingRow(profile.name + (isDefault ? ` — ${t('agent.pairProfile.default')}` : ''), summary, controls);
      row.dataset.pairProfileId = profile.id;
      return row;
    });
    return node('div', { className: 'view-stack' },
      settingsPanel(t('agent.pairProfile.title'), t('agent.pairProfile.settingsHelp'),
        node('div', { className: 'panel-body pair-profile-actions' },
          actionButton(t('agent.pairProfile.new'), () => openRoomDialog('', ''), 'primary-button'),
          actionButton(t('agent.pairProfile.refresh'), refreshPairProfileSettings, 'secondary-button')),
        state.agentPairProfilesError ? node('p', { className: 'form-error', role: 'alert', textContent: state.agentPairProfilesError }) : null,
        ...rows,
        !rows.length ? node('p', { className: 'panel-body', textContent: catalog ? t('agent.pairProfile.empty') : t('agent.pairProfile.loading') }) : null,
      ),
    );
  }

  function populatePairProfilePicker(id = '') {
    const catalog = state.agentPairProfiles;
    $('room-pair-profile').replaceChildren(
      node('option', { value: '', textContent: t('agent.pairProfile.serviceDefaults') }),
      ...(catalog?.profiles || []).map((profile) => node('option', {
        value: profile.id, textContent: profile.name + (profile.id === catalog.default_profile_id ? ` — ${t('agent.pairProfile.default')}` : ''),
      })),
    );
    $('room-pair-profile').value = id;
    $('pair-profile-update').disabled = !id;
  }

  function applyPairProfile(id) {
    const profile = state.agentPairProfiles?.profiles.find((entry) => entry.id === id);
    if (id && !profile) throw new Error(t('agent.pairProfile.notFound'));
    state.pairProfileID = id;
    for (const actor of ['claude', 'codex']) populateAgentControls(actor, (profile?.agents || state.agentCatalog?.defaults)?.[actor]);
    $('pair-profile-name').value = profile?.name || '';
    $('pair-profile-default').checked = Boolean(id && id === state.agentPairProfiles?.default_profile_id);
    $('pair-profile-update').disabled = !id;
    syncCollaborationControls();
  }

  async function saveCurrentPairProfile(update = false) {
    if (!state.pairProfileReady) return;
    const name = $('pair-profile-name').value;
    if (!validRoomName(name, false)) {
      showFormError('room-form-error', t('agent.pairProfile.invalidName'));
      $('pair-profile-save-options').open = true;
      $('pair-profile-name').focus();
      return;
    }
    const id = update ? state.pairProfileID : '';
    if (update && !id) return;
    const revision = state.roomDialogRevision;
    const editor = state.pairProfileMode;
    const input = { name: name.trim(), is_default: $('pair-profile-default').checked,
      agents: { claude: readAgentSelection('claude'), codex: readAgentSelection('codex') } };
    if (state.pairProfileBusy) return;
    const buttons = ['pair-profile-save-new', 'pair-profile-update', 'room-submit'];
    buttons.forEach((key) => { $(key).disabled = true; });
    try {
      hideFormError('room-form-error');
      const catalog = await mutateAgentPairProfiles(id ? '/' + encodeURIComponent(id) : '', {
        method: id ? 'PUT' : 'POST', body: JSON.stringify(input),
      });
      if (revision !== state.roomDialogRevision || !state.authenticated || !$('room-dialog').open) return;
      const saved = catalog.profiles.find((entry) => id ? entry.id === id : entry.name === input.name);
      state.pairProfileID = saved?.id || '';
      populatePairProfilePicker(state.pairProfileID);
      if (editor) {
        closeDialog('room-dialog');
        if (state.route.name === 'settings') renderSettings();
      }
      toast(t('agent.pairProfile.saved'), input.name, 'success');
    } catch (error) {
      if (revision === state.roomDialogRevision) showFormError('room-form-error', error.message);
    } finally {
      if (revision === state.roomDialogRevision) {
        $('room-submit').disabled = false;
        $('pair-profile-save-new').disabled = false;
        $('pair-profile-update').disabled = !state.pairProfileID;
      }
    }
  }

  // Selects must preserve explicit values from a saved profile, including a
  // newer effort level that was not in this build's suggestion list.
  function setAgentSelectValue(select, value) {
    if (value && ![...select.options].some((option) => option.value === value)) {
      select.append(node('option', { value, textContent: value }));
    }
    select.value = value || '';
  }

  function providerOptionValue(ref) {
	return JSON.stringify(ref || { source: 'native' });
  }

  function setProviderSelection(provider, requested) {
    const value = requested || providerOptionValue({ source: 'native' });
    let option = [...provider.options].find((entry) => entry.value === value);
    if (!option) {
      // A catalog refresh may remove a Profile. Preserve the explicit reference
      // and require a human choice rather than silently switching Providers.
      option = node('option', { value, textContent: t('agent.selectedProviderUnavailable'), disabled: true });
      provider.append(option);
    }
    provider.value = value;
    provider.setCustomValidity(option.disabled ? t('agent.selectedProviderUnavailable') : '');
  }

  function selectedProviderRef(actor) {
	try { return JSON.parse($(`${actor}-provider`).value); }
	catch { return { source: 'native' }; }
  }

  function runtimeCatalogEntry(runtime) {
	return (state.agentCatalog?.runtimes || []).find((entry) => entry.runtime === runtime);
  }

  // resetDependent is only for Runtime changes: it rewinds Provider to native
  // and clears Model. A Provider change must pass false so the selected Profile stays.
  function syncAgentProviderAndModels(actor, resetDependent = false) {
	const runtime = $(`${actor}-runtime`).value;
	const provider = $(`${actor}-provider`);
	const previous = resetDependent ? providerOptionValue({ source: 'native' }) : provider.value;
	const options = [node('option', { value: providerOptionValue({ source: 'native' }), textContent: t('agent.nativeProvider') })];
	for (const profile of state.agentCatalog?.profiles || []) {
	  if (profile.runtime !== runtime) continue;
	  const label = `${profile.name}${profile.supported ? '' : ` — ${profileDisabledReason(profile)}`}`;
	  options.push(node('option', { value: providerOptionValue(profile.provider), textContent: label, disabled: !profile.supported }));
	}
	provider.replaceChildren(...options);
	setProviderSelection(provider, previous);
	const selected = selectedProviderRef(actor);
	const profile = (state.agentCatalog?.profiles || []).find((entry) => providerOptionValue(entry.provider) === providerOptionValue(selected));
	const runtimeEntry = runtimeCatalogEntry(runtime);
	const models = new Set([...(runtimeEntry?.default_models || []), ...(profile?.models || [])]);
	$(`${actor}-model-options`).replaceChildren(...[...models].filter(Boolean).sort().map((modelName) => node('option', { value: modelName })));
	if (resetDependent) {
      $(`${actor}-model`).value = '';
      syncAgentPolicy(actor);
      $(`${actor}-permission-mode`).value = runtime === 'codex' ? '' : 'yolo';
      $(`${actor}-approval-policy`).value = runtime === 'codex' ? 'yolo' : '';
      $(`${actor}-sandbox`).value = runtime === 'codex' ? 'danger-full-access' : runtime === 'grok' ? 'off' : '';
    }
	const providerDiagnostic = $(`${actor}-provider-diagnostic`);
	providerDiagnostic.textContent = provider.validationMessage || (state.agentCatalog?.provider_error
	  ? (window.PairRoomI18n?.errorMessage(state.agentCatalog.provider_error) || state.agentCatalog.provider_error.error || '')
	  : '');
	providerDiagnostic.classList.toggle('runtime-unavailable', Boolean(provider.validationMessage || state.agentCatalog?.provider_error));
	syncAgentPolicy(actor);
  }

  function syncAgentPolicy(actor) {
	const runtime = $(`${actor}-runtime`).value;
	document.querySelectorAll(`[data-actor="${actor}"] [data-policy-for]`).forEach((field) => {
	  field.hidden = !field.dataset.policyFor.split(',').includes(runtime);
	});
	const permission = $(`${actor}-permission-mode`);
	const previousPermission = permission.value;
	const permissionValues = runtime === 'claude'
	  ? ['default', 'manual', 'acceptEdits', 'plan', 'auto', 'dontAsk', 'bypassPermissions', 'bypass', 'always-approve', 'yolo']
	  : runtime === 'grok' ? ['default', 'ask', 'acceptEdits', 'plan', 'auto', 'dontAsk', 'bypassPermissions', 'always-approve', 'yolo'] : [];
	permission.replaceChildren(
	  node('option', { value: '', textContent: t('common.inherit') }),
	  ...permissionValues.map((value) => node('option', { value, textContent: value })),
	);
	permission.value = permissionValues.includes(previousPermission) ? previousPermission : '';
	const sandbox = $(`${actor}-sandbox`);
	const previousSandbox = sandbox.value;
	const sandboxValues = runtime === 'codex'
	  ? ['read-only', 'workspace-write', 'danger-full-access']
	  : runtime === 'grok' ? ['read-only', 'workspace', 'strict', 'off'] : [];
	sandbox.replaceChildren(
	  node('option', { value: '', textContent: t('common.inherit') }),
	  ...sandboxValues.map((value) => node('option', { value, textContent: value })),
	);
	sandbox.value = sandboxValues.includes(previousSandbox) ? previousSandbox : '';
	const bindingKind = runtime === 'codex' ? t('room.thread') : t('room.session');
	$(`${actor}-binding-label`).textContent = t('agent.bindingId', { runtime: runtimeCatalogEntry(runtime)?.display_name || runtime, binding: bindingKind });
  }

  function profileDisabledReason(profile) {
	const key = profile?.reason_code ? `agent.reason.${profile.reason_code}` : '';
	return key && window.i18next?.exists(key) ? t(key) : (profile?.disabled_reason || t('agent.unavailable'));
  }

  function populateAgentControls(actor, selection) {
	const runtime = $(`${actor}-runtime`);
	runtime.replaceChildren(...(state.agentCatalog?.runtimes || []).map((entry) => node('option', {
	  value: entry.runtime,
	  textContent: `${entry.display_name}${entry.available ? '' : ` — ${t('agent.unavailable')}`}`,
	  disabled: !entry.available,
	})));
    setAgentSelectValue(runtime, selection?.runtime || (actor === 'claude' ? 'claude' : 'codex'));
    runtime.setCustomValidity(runtimeCatalogEntry(runtime.value)?.available ? '' : t('agent.unavailable'));
	const runtimeEntry = runtimeCatalogEntry(runtime.value);
	const diagnostic = $(`${actor}-runtime-diagnostic`);
	diagnostic.textContent = runtimeEntry?.diagnostic || (runtimeEntry?.version ? `v${runtimeEntry.version}` : '');
	diagnostic.classList.toggle('runtime-unavailable', !runtimeEntry?.available);
	syncAgentProviderAndModels(actor, false);
	const wantedProvider = providerOptionValue(selection?.provider || { source: 'native' });
	setProviderSelection($(`${actor}-provider`), wantedProvider);
	$(`${actor}-model`).value = selection?.model || '';
    setAgentSelectValue($(`${actor}-effort`), selection?.effort);
	$(`${actor}-permission-mode`).value = selection?.permission_mode || '';
    setAgentSelectValue($(`${actor}-approval-policy`), selection?.approval_policy);
	$(`${actor}-sandbox`).value = selection?.sandbox || '';
	$(`${actor}-instructions`).value = selection?.instructions || '';
	syncAgentProviderAndModels(actor, false);
  }

  function readAgentSelection(actor) {
	const runtime = $(`${actor}-runtime`).value;
	return {
	  runtime,
	  provider: selectedProviderRef(actor),
	  model: $(`${actor}-model`).value.trim(),
	  effort: $(`${actor}-effort`).value,
	  instructions: $(`${actor}-instructions`).value.trim(),
	  permission_mode: runtime === 'codex' ? '' : $(`${actor}-permission-mode`).value.trim(),
	  approval_policy: runtime === 'codex' ? $(`${actor}-approval-policy`).value : '',
	  sandbox: runtime === 'claude' ? '' : $(`${actor}-sandbox`).value,
	};
  }

  async function openRoomDialog(projectID = '', profileID = null) {
    const editor = profileID !== null;
    const projects = (state.snapshot?.projects || []).filter((project) => project.available);
    if (!editor && !projects.length) {
      toast(t("ui.noAvailableProject"), t("ui.pleaseRegisterOrRepairAGitWorktreeFirst"), 'warning');
      return;
    }
    const revision = ++state.roomDialogRevision;
    state.pairProfileMode = editor;
    state.pairProfileReady = false;
    state.pairProfileID = profileID || '';
    const select = $('room-project-id');
    select.replaceChildren(...projects.map((project) => node('option', { value: project.id, textContent: `${projectName(project)} — ${project.root}` })));
    select.value = projects.some((project) => project.id === projectID) ? projectID : (projects[0]?.id || '');
    $('room-name').value = '';
    $('room-collaboration-mode').value = 'default';
    $('room-collaboration-instructions').value = '';
    document.querySelectorAll('#room-dialog [data-room-only]').forEach((element) => { element.hidden = editor; });
    $('pair-profile-picker').hidden = editor;
    $('pair-profile-save-actions').hidden = editor;
    $('pair-profile-save-options').open = editor;
    $('pair-profile-name').value = '';
    $('pair-profile-default').checked = false;
    $('room-pair-profile').disabled = true;
    $('pair-profile-save-new').disabled = true;
    $('pair-profile-update').disabled = true;
    syncCollaborationControls();
    document.querySelector('input[name="claude-mode"][value="new"]').checked = true;
    document.querySelector('input[name="codex-mode"][value="new"]').checked = true;
    $('claude-session-id').value = '';
    $('codex-session-id').value = '';
    syncBindingInputs();
    hideFormError('room-form-error');
    setRenderedText('room-dialog-title', editor ? t(profileID ? 'agent.pairProfile.edit' : 'agent.pairProfile.new')
      : t("ui.createRoomInValue", { value0: projectName(projects.find((project) => project.id === select.value)) }));
    syncPairProfileDialogLabels();
    showDialog('room-dialog');
    $('room-submit').disabled = true;
    try {
      const [, profiles] = await Promise.all([loadAgentCatalog(), loadAgentPairProfiles(true)]);
      if (revision !== state.roomDialogRevision || !$('room-dialog').open || !state.authenticated) return;
      const selectedID = editor ? profileID : (profiles.default_profile_id || '');
      populatePairProfilePicker(selectedID);
      applyPairProfile(selectedID);
      state.pairProfileReady = true;
      $('room-pair-profile').disabled = false;
      $('pair-profile-save-new').disabled = false;
      $('room-submit').disabled = false;
      queueMicrotask(() => $(editor ? 'pair-profile-name' : 'room-name').focus());
    } catch (error) {
      if (revision === state.roomDialogRevision) showFormError('room-form-error', error.message);
    }
  }

  function syncPairProfileDialogLabels() {
    if (state.pairProfileMode) setRenderedText('room-dialog-title', t(state.pairProfileID ? 'agent.pairProfile.edit' : 'agent.pairProfile.new'));
    setRenderedText('pair-profile-help', t(state.pairProfileMode ? 'agent.pairProfile.editorHelp' : 'agent.pairProfile.templateHelp'));
    if (!state.pairProfileBusy && !state.busyButtons.has($('room-submit'))) {
      setRenderedText('room-submit', t(state.pairProfileMode ? 'agent.pairProfile.save' : 'ui.createRoomAtomically'));
    }
  }

  function syncCollaborationControls() {
    const custom = state.pairProfileMode || $('room-collaboration-mode').value === 'custom';
    $('collaboration-custom-field').hidden = !custom;
    $('collaboration-default-details').hidden = custom;
    $('room-collaboration-instructions').required = custom;
    $('collaboration-help').textContent = t(custom ? 'room.collaboration.customHelp' : 'room.collaboration.defaultHelp');
    $('collaboration-default-instructions').textContent = state.agentCatalog?.collaboration_default?.instructions || t('room.collaboration.defaultHelp');
    setRenderedText('claude-responsibility-label', custom ? t('agent.agent1') : t('room.collaboration.leadSlot'));
    setRenderedText('codex-responsibility-label', custom ? t('agent.agent2') : t('room.collaboration.executorSlot'));
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
    if (state.pairProfileMode) return saveCurrentPairProfile(Boolean(state.pairProfileID));
    if (state.pairProfileBusy || $('room-submit').disabled) return;
    const projectID = $('room-project-id').value;
    const name = $('room-name').value.trim();
    if (!validRoomName($('room-name').value, true)) {
      showFormError('room-form-error', t('room.invalidName'));
      return;
    }
    const mode = $('room-collaboration-mode').value;
    const instructions = mode === 'custom' ? $('room-collaboration-instructions').value.trim() : '';
    if (mode === 'custom' && (!instructions || new TextEncoder().encode(instructions).length > 16384)) {
      showFormError('room-form-error', t('room.collaboration.invalidInstructions')); return;
    }
    const collaboration = { mode, ...(mode === 'custom' ? { instructions } : {}) };
    const bindings = {};
    for (const actor of ['claude', 'codex']) {
      if (!$(`${actor}-runtime`).reportValidity() || !$(`${actor}-provider`).reportValidity()) return;
      const mode = document.querySelector(`input[name="${actor}-mode"]:checked`)?.value || 'new';
      const sessionID = $(`${actor}-session-id`).value.trim();
      if (mode === 'existing' && !sessionID) {
		showFormError('room-form-error', t("ui.valueIdIsRequired", { value0: $(`${actor}-binding-label`).textContent.replace(/ ID$/, '') }));
        return;
      }
	  bindings[actor] = mode === 'existing' ? { mode, session_id: sessionID } : { mode };
	}
	const agents = { claude: readAgentSelection('claude'), codex: readAgentSelection('codex') };
    await withBusy($('room-submit'), async () => {
      try {
        hideFormError('room-form-error');
		await api(`/api/v1/projects/${encodeURIComponent(projectID)}/rooms`, { method: 'POST', body: JSON.stringify({ name, bindings, agents, collaboration }) });
        closeDialog('room-dialog');
        toast(t("ui.roomCreated"), t("ui.agentBindingsCompletedAtomicVerification"), 'success');
        await refresh({ forceRender: true, fresh: true });
        navigate(`#/projects/${encodeURIComponent(projectID)}`);
      } catch (error) {
        showFormError('room-form-error', error.message);
      }
    });
  }

  function validRoomName(value, optional) {
    const trimmed = value.trim();
    return (optional || Boolean(trimmed)) && new TextEncoder().encode(trimmed).length <= 160
      && !/[\u0000-\u001f\u007f-\u009f]/u.test(value);
  }

  function closeRoomContextMenu(restoreFocus = false) {
    const roomID = state.contextRoomID;
    const trigger = state.contextTrigger?.isConnected ? state.contextTrigger
      : Array.from(document.querySelectorAll('.tree-room[data-room-id], .room-tab[data-room-id]')).find(node => node.dataset.roomId === roomID);
    $('room-context-menu').hidden = true;
    state.contextRoomID = '';
    state.contextTrigger = null;
    state.contextScrollPositions.clear();
    if (restoreFocus && trigger) (trigger.matches('button') ? trigger : trigger.querySelector('button'))?.focus({ preventScroll: true });
  }

  function positionRoomContextMenu(left, top) {
    // Mutate only the same-origin, shipped stylesheet: no inline style
    // attributes, injected style elements, or relaxation of Management CSP.
    const sheet = $('management-styles')?.sheet;
    if (!sheet) return; // Static top/left keep the action reachable on failure.
    const previous = state.contextPositionRule;
    if (previous?.parentStyleSheet === sheet) {
      const index = Array.from(sheet.cssRules).indexOf(previous);
      if (index >= 0) sheet.deleteRule(index);
    }
    const index = sheet.insertRule(`#room-context-menu { left: ${left}px; top: ${top}px; }`, sheet.cssRules.length);
    state.contextPositionRule = sheet.cssRules[index];
  }

  function openRoomContextMenu(event, keyboard = false) {
    const trigger = event.target.closest?.('.tree-room[data-room-id], .room-tab[data-room-id], .room-row[data-room-id]');
    const room = trigger && roomByID(trigger.dataset.roomId);
    if (!room || !state.authenticated) return;
    event.preventDefault();
    event.stopPropagation();
    state.contextRoomID = room.id;
    state.contextTrigger = trigger.matches('button') ? trigger : trigger.querySelector('button');
    state.contextScrollPositions.clear();
    for (let ancestor = trigger; ancestor; ancestor = ancestor.parentElement) {
      state.contextScrollPositions.set(ancestor, [ancestor.scrollLeft, ancestor.scrollTop]);
    }
    const menu = $('room-context-menu');
    $('context-room-name').textContent = room.name;
    menu.hidden = false;
    const rect = trigger.getBoundingClientRect();
    const x = !keyboard && Number.isFinite(event.clientX) ? event.clientX : rect.left;
    const y = !keyboard && Number.isFinite(event.clientY) ? event.clientY : rect.bottom;
    positionRoomContextMenu(
      Math.max(8, Math.min(x, window.innerWidth - menu.offsetWidth - 8)),
      Math.max(8, Math.min(y, window.innerHeight - menu.offsetHeight - 8)),
    );
    $('context-rename-room').focus({ preventScroll: true });
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
    if (!validRoomName($('rename-room-name').value, false)) {
      showFormError('rename-form-error', t('room.invalidName'));
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
        toast(t("ui.roomRenamed"), t("ui.changesAreCommittedAtTheSafeTurnBoundary"), 'success');
        await refresh({ forceRender: true, fresh: true });
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
	  const label = actor === 'claude' ? t('room.claudeSession') : t('room.codexThread');
      const field = node('fieldset', { className: 'binding-card', 'data-complete-actor': actor },
        node('legend', {}, node('span', { className: `agent-avatar ${actor}`, textContent: actor === 'claude' ? '1' : '2' }), node('span', {}, node('strong', { textContent: label }), node('small', { textContent: t('room.missingBinding') }))),
        node('label', { className: 'choice-card' }, node('input', { type: 'radio', name: `complete-${actor}-mode`, value: 'new', checked: true }), node('span', {}, node('strong', { textContent: t("ui.createValue", { value0: (label) }) }), node('small', { textContent: t("ui.identityIsSolidifiedOnTheFirstRealTurn") }))),
        node('label', { className: 'choice-card' }, node('input', { type: 'radio', name: `complete-${actor}-mode`, value: 'existing' }), node('span', {}, node('strong', { textContent: t("ui.reuseExisting") }), node('small', { textContent: t("ui.mustNotBeOccupiedByOtherRooms") }))),
        node('label', { className: 'session-field' }, node('span', { textContent: `${label} ID` }), node('input', { id: `complete-${actor}-session`, type: 'text', placeholder: t("ui.pasteValueId", { value0: (label) }), autocomplete: 'off', disabled: true }))
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
    container.append(node('div', { className: 'callout boundary' }, node('strong', { textContent: t('room.atomicCompletion') }), node('span', { textContent: t("ui.whenAnyExistingIdIsInvalidUnrecoverableOrAlreadyOccupiedTheEntire") })));
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
        showFormError('binding-form-error', t("ui.valueIdIsRequired", { value0: (actor === 'claude' ? t('room.claudeSession') : t('room.codexThread')) }));
        return;
      }
      bindings[actor] = mode === 'existing' ? { mode, session_id: sessionID } : { mode };
    }
    const button = event.submitter || event.currentTarget.querySelector('[type="submit"]');
    await withBusy(button, async () => {
      try {
        await api(`/api/v1/rooms/${encodeURIComponent(state.bindingRoomID)}/bindings`, { method: 'POST', body: JSON.stringify({ bindings }) });
        closeDialog('binding-dialog');
        toast(t("ui.bindingsCompleted"), t("ui.legacyRoomIsNowSafeToActivate"), 'success');
        await refresh({ forceRender: true, fresh: true });
      } catch (error) {
        showFormError('binding-form-error', error.message);
      }
    });
  }

  function archiveRoom(room) {
    openConfirm({
      eyebrow: t('room.archiveRoomUpper'),
      title: t("ui.archiveValue", { value0: (room.name) }),
      message: t("ui.theActiveTurnStopsFirstThenTheRuntimeIsSuspendedAndThe"),
      detail: t("ui.eventLogAttachmentsRolesDraftsUnreadStatusAndBindingIdentityOnBoth"),
      label: t("ui.archiveRoom"),
      tone: 'danger',
      action: async () => {
        await api(`/api/v1/rooms/${encodeURIComponent(room.id)}/archive`, { method: 'POST' });
        toast(t("ui.roomArchived"), t("ui.historyAndBindingIdentityHaveBeenPreserved"), 'success');
        await refresh({ forceRender: true, fresh: true });
      },
    });
  }

  async function refreshProject(project) {
    try {
      const refreshed = await api(`/api/v1/projects/${encodeURIComponent(project.id)}/refresh`, { method: 'POST' });
      if (refreshed.available) {
        toast(t("ui.projectAvailable"), t("ui.canonicalWorktreeHasBeenRevalidated"), 'success');
      } else {
        toast(t("ui.projectRemainsUnavailable"), refreshed.diagnostic || t("ui.canonicalWorktreeIsCurrentlyInaccessible"), 'warning');
      }
      await refresh({ forceRender: true, fresh: true });
    } catch (error) {
      toast(t("ui.projectCheckFailed"), error.message, 'error');
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
      toast(t("ui.batchLimitReached"), t("ui.selectAtMostValueRooms", { value0: (MAX_ROOM_BATCH_SIZE) }), 'warning');
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
    if (skipped) toast(t("ui.someRoomsWereNotSelected"), t("ui.processAtMostValuePerRequestValueMoreCanBeHandledIn", { value0: (MAX_ROOM_BATCH_SIZE), value1: (skipped) }), 'warning');
    render();
  }
  function roomSelectionToggleButton(candidates, scopeLabel) {
    const eligible = uniqueRooms(candidates);
    const selected = eligible.filter((room) => state.selectedRoomIDs.has(room.id));
    const allSelected = eligible.length > 0 && selected.length === eligible.length;
    const label = t(allSelected ? 'workspace.deselectAll' : 'workspace.selectAll', { count: eligible.length });
    const button = actionButton(label, () => toggleRoomSelectionGroup(eligible), `filter-chip ${selected.length ? 'active' : ''}`, eligible.length === 0);
    button.title = scopeLabel;
    return button;
  }
  function roomClearSelectionButton() {
    const count = state.selectedRoomIDs.size;
    if (!count) return null;
    return actionButton(t("ui.clearSelectionValue", { value0: (count) }), () => {
      state.selectedRoomIDs.clear();
      render();
    }, 'secondary-button outline');
  }
  function roomBatchArchiveButton(rooms) {
    const count = rooms.length;
    return actionButton(t("ui.batchArchiveValue", { value0: (count) }), () => confirmRoomArchive(rooms), 'secondary-button outline', count === 0);
  }
  function roomBatchRemovalButton(rooms) {
    const count = rooms.length;
    return actionButton(t("ui.batchDeleteValue", { value0: (count) }), () => confirmRoomRemoval(rooms), 'danger-button outline', count === 0);
  }
  function projectRemovalButton(project, roomCount, compact = false) {
    if (!state.snapshot?.capabilities?.project_removal) return null;
    const disabled = roomCount > 0;
    const explanation = disabled
      ? t("ui.stillContainsValueRoomsIncludingArchivedRoomsArchiveAndPermanentlyDeleteThem", { value0: (roomCount) })
      : t("ui.removeOnlyTheServiceRegistryEntryDoNotDeleteTheGitWorktree");
    return node('button', {
      type: 'button',
      className: `danger-button outline${compact ? ' compact-button' : ''}`,
      textContent: t("ui.logOutProject"),
      disabled,
      title: explanation,
      'aria-label': t("ui.unregisterProjectValueValue", { value0: (projectName(project)), value1: (explanation) }),
      onClick: () => removeProject(project),
    });
  }
  function removeProject(project) {
    const roomCount = (state.snapshot?.rooms || []).filter((room) => room.project_id === project.id).length;
    if (roomCount > 0) {
      toast(t("ui.cannotUnregisterProject"), t("ui.stillContainsValueRoomsIncludingArchivedRoomsArchiveAndPermanentlyDeleteThem", { value0: (roomCount) }), 'warning');
      return;
    }
    openConfirm({
      eyebrow: t('room.unregisterProjectUpper'),
      title: t("ui.unregisterValue", { value0: (projectName(project)) }),
      message: t("ui.unregisterThisEmptyProjectFromTheServiceRegistry"),
      detail: t("ui.gitWorktreeOrVendorSessionThreadWillNotBeDeletedTheBackend"),
      label: t("ui.logOutProject"),
      tone: 'danger',
      confirmation: project.id,
      confirmationLabel: t("ui.enterFullProjectIdToConfirm"),
      action: async () => {
        await api(`/api/v1/projects/${encodeURIComponent(project.id)}`, {
          method: 'DELETE',
          body: JSON.stringify({ confirm_project_id: project.id }),
        });
        toast(t("ui.projectUnregistered"), t("ui.gitWorktreeAndExternalDataAreNotModified"), 'success');
        await refresh({ forceRender: true, fresh: true });
        navigate('#/projects');
      },
    });
  }
  function confirmRoomArchive(candidates) {
    const rooms = eligibleActiveRooms(candidates);
    if (!rooms.length) {
      toast(t("ui.noRoomsCanBeArchived"), t("ui.pleaseSelectAtLeastOneActiveRoom"), 'warning');
      return;
    }
    if (rooms.length > MAX_ROOM_BATCH_SIZE) {
      toast(t("ui.batchLimitExceeded"), t("ui.processAtMostValueRoomsPerRequest", { value0: (MAX_ROOM_BATCH_SIZE) }), 'warning');
      return;
    }
    const preview = window.PairRoomI18n.formatList(rooms.slice(0, 6).map((room) => room.name));
    const remaining = rooms.length > 6 ? t("ui.plusValueMore", { value0: (rooms.length - 6) }) : '';
    openConfirm({
      eyebrow: rooms.length === 1 ? t('room.archiveRoomUpper') : t('room.batchArchiveRoomsUpper'),
      title: rooms.length === 1 ? t("ui.archiveValue", { value0: (rooms[0].name) }) : t("ui.archiveValueRooms", { value0: (rooms.length) }),
      message: t("ui.valueValueArchivedRoomsCanBeRestoredOrPermanentlyDeletedInA", { value0: (preview), value1: (remaining) }),
      detail: t("ui.archivingPreservesTheEventLogAttachmentsAndAgentBindingsBatchRequestsRun"),
      label: rooms.length === 1 ? t("ui.archiveRoom") : t("ui.archiveValueRoomse83d2c9", { value0: (rooms.length) }),
      tone: 'primary',
      action: async () => {
        const response = await api('/api/v1/rooms/batch-archive', {
          method: 'POST',
          body: JSON.stringify({ room_ids: rooms.map((room) => room.id) }),
        });
        if (!Array.isArray(response.results)) throw new Error(t("ui.batchArchiveResponseIsMissingResults"));
        const results = response.results;
        const succeeded = results.filter((item) => item.status === 'archived' || item.status === 'already_archived');
        const failed = results.filter((item) => item.status !== 'archived' && item.status !== 'already_archived');
        succeeded.forEach((item) => state.selectedRoomIDs.add(item.room_id));
        if (succeeded.length) state.filters.showArchived = true;
        if (failed.length) {
          const detail = failed.slice(0, 2).map((item) => {
            const room = rooms.find((candidate) => candidate.id === item.room_id);
            return `${room?.name || item.room_id}: ${item.error || item.code || t("ui.archivingFailed")}`;
          }).join('；');
          const more = failed.length > 2 ? t("ui.valueMoreFailed", { value0: (failed.length - 2) }) : '';
          toast(
            succeeded.length ? t("ui.batchArchivingPartiallyCompleted") : t("ui.batchArchivingNotCompleted"),
            t("ui.valueSucceededAndValueFailedValueValue", { value0: (succeeded.length), value1: (failed.length), value2: (detail), value3: (more) }),
            succeeded.length ? 'warning' : 'error'
          );
        } else {
          toast(t("ui.roomArchived"), t("ui.archivedValueRoomsTheyRemainSelectedSoYouCanContinueWithBatch", { value0: (succeeded.length) }), 'success');
        }
        await refresh({ forceRender: true, fresh: true });
      },
    });
  }
  function confirmRoomRemoval(candidates) {
    const rooms = eligibleArchivedRooms(candidates);
    if (!rooms.length) {
      toast(t("ui.noRoomsCanBeDeleted"), t("ui.pleaseSelectAtLeastOneArchivedRoom"), 'warning');
      return;
    }
    if (rooms.length > MAX_ROOM_BATCH_SIZE) {
      toast(t("ui.batchLimitExceeded"), t("ui.processAtMostValueRoomsPerRequest", { value0: (MAX_ROOM_BATCH_SIZE) }), 'warning');
      return;
    }
    const preview = window.PairRoomI18n.formatList(rooms.slice(0, 6).map((room) => room.name));
    const remaining = rooms.length > 6 ? t("ui.plusValueMore", { value0: (rooms.length - 6) }) : '';
    openConfirm({
      eyebrow: rooms.length === 1 ? t('room.permanentlyRemoveRoomUpper') : t('room.batchRemoveRoomsUpper'),
      title: rooms.length === 1 ? t("ui.permanentlyDeleteValue", { value0: (rooms[0].name) }) : t("ui.permanentlyDeleteValueRooms", { value0: (rooms.length) }),
      message: t("ui.valueValueThisActionCannotBeUndone", { value0: (preview), value1: (remaining) }),
      detail: t("ui.pairroomManagedEventLogsAttachmentsAndRoomDataAreDeletedGitWorktrees"),
      label: rooms.length === 1 ? t("ui.clearRoomPermanently") : t("ui.permanentlyDeleteValueRooms891a8ae", { value0: (rooms.length) }),
      tone: 'danger',
      acknowledgement: t("ui.iUnderstandThatPairroomManagementDataForTheSelectedValueRoomsWill", { value0: (rooms.length) }),
      action: async () => {
        const response = await api('/api/v1/rooms/batch-delete', {
          method: 'POST',
          body: JSON.stringify({
            room_ids: rooms.map((room) => room.id),
            acknowledge_data_loss: true,
          }),
        });
        if (!Array.isArray(response.results)) throw new Error(t("ui.batchCleanupResponseIsMissingResults"));
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
            return `${room?.name || item.room_id}: ${item.error || item.code || t("ui.deleteFailed")}`;
          }).join('；');
          const more = failed.length > 2 ? t("ui.valueMoreFailed", { value0: (failed.length - 2) }) : '';
          toast(
            succeeded.length ? t("ui.batchCleanupPartiallyCompleted") : t("ui.batchCleanupNotCompleted"),
            t("ui.valueSucceededAndValueFailedValueValue", { value0: (succeeded.length), value1: (failed.length), value2: (detail), value3: (more) }),
            succeeded.length ? 'warning' : 'error'
          );
        } else if (cleanupPending) {
          toast(t("ui.roomRemovedPhysicalCleanupNeedsRetry"), t("ui.processedValueValueQuarantinedCleanupItemsCanBeRetriedInSettings", { value0: (succeeded.length), value1: (cleanupPending) }), 'warning');
        } else if (retainedExternal) {
          toast(t("ui.roomCleanupComplete"), t("ui.processedValueValueImportedExternalDirectoriesRemainInPlace", { value0: (succeeded.length), value1: (retainedExternal) }), 'success');
        } else {
          toast(t("ui.roomPermanentlyDeleted"), t("ui.permanentlyDeletedValueArchivedRooms", { value0: (succeeded.length) }), 'success');
        }
        await refresh({ forceRender: true, fresh: true });
      },
    });
  }
  async function retryRoomDeletionCleanup() {
    try {
      const maintenance = await api('/api/v1/maintenance/room-deletions/retry', { method: 'POST' });
      if (maintenance.pending_cleanup) {
        toast(t("ui.cleanupItemsRemain"), maintenance.diagnostic || t("ui.valueQuarantinedItemsStillNeedAttention", { value0: (maintenance.pending_cleanup) }), 'warning');
      } else {
        toast(t("ui.roomCleanupCompleted"), t("ui.theDeletionQuarantineHasBeenCleared"), 'success');
      }
      await refresh({ forceRender: true, fresh: true });
    } catch (error) {
      toast(t("ui.roomCleanupRetryFailed"), error.message, 'error');
    }
  }
  async function restoreRoom(room) {
    try {
      await api(`/api/v1/rooms/${encodeURIComponent(room.id)}/restore`, { method: 'POST' });
      toast(t("ui.roomRestored"), t("ui.theFullHistoryAndBindingIdentityAreAvailableAgain"), 'success');
      await refresh({ forceRender: true, fresh: true });
    } catch (error) {
      toast(t("ui.restoreFailed"), error.message, 'error');
    }
  }

  function suspendRoom(room, runtime) {
    const queued = runtime.phase === 'queued';
    openConfirm({
      eyebrow: queued ? t('room.cancelActivationUpper') : t('room.suspendRuntimeUpper'),
      title: queued ? t("ui.cancelActivationQueueForValue", { value0: (room.name) }) : t("ui.suspendTheRuntimeForValue", { value0: (room.name) }),
      message: queued ? t("ui.theRoomReturnsToSuspendedAndCanReEnterTheQueueThe") : t("ui.onlyRuntimesWithNoActiveTurnWillShutDownTheVendorProcess"),
      detail: queued ? '' : t("ui.ifRoomIsWorkingTheBackendWillReturnAConflictAndWill"),
      label: queued ? t("ui.cancelQueue") : t("ui.hangRuntime"),
      tone: 'danger',
      action: async () => {
        await api(`/api/v1/rooms/${encodeURIComponent(room.id)}/suspend`, { method: 'POST' });
        toast(queued ? t("ui.queueCanceled") : t("ui.runtimeHasHung"), queued ? t("ui.roomRemainsRecoverable") : t("ui.capacityHasBeenSafelyReleased"), 'success');
        await refresh({ forceRender: true, fresh: true });
      },
    });
  }

  async function openRoom(roomID) {
    const room = roomByID(roomID);
    if (!room) return;
    if (room.lifecycle === 'archived') {
      toast(t("ui.roomArchived"), t("ui.canOnlyBeOpenedAfterRecovery"), 'warning');
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
    if (state.openingBrowsers.has(roomID)) return;
    const room = roomByID(roomID);
    if (!room || room.lifecycle === 'archived') {
      toast(t("ui.couldNotOpenInBrowser"), t("ui.theArchiveRoomDoesNotHaveAnIndependentRuntimeUrlPleaseRestore"), 'warning');
      return;
    }
    if (roomHasBlockingPendingBindings(room)) {
      completeBindings(room);
      return;
    }
    const generation = state.sessionGeneration;
    state.openingBrowsers.add(roomID);
    const deadline = Date.now() + 60000;
    toast(t("ui.preparingTheSystemBrowser"), t("ui.waitForTheRuntimeToBeReadyBeforeOpeningIt"), 'success');
    try {
      while (Date.now() < deadline && generation === state.sessionGeneration) {
        const status = await api(`/api/v1/rooms/${encodeURIComponent(roomID)}/activate`, { method: 'POST' });
        if (generation !== state.sessionGeneration) return;
        if (status.phase === 'failed') throw new Error(status.last_error || t("ui.runtimeFailed"));
        if (status.phase === 'active') {
          // The activation response, not a potentially older polling snapshot,
          // is the readiness receipt. The server owns the one-time browser URL.
          await api(`/api/v1/rooms/${encodeURIComponent(roomID)}/open-browser`, { method: 'POST' });
          if (generation !== state.sessionGeneration) return;
          toast(t("ui.openedInTheSystemBrowser"), t("ui.willNotLeaveTheCurrentWorkbench"), 'success');
          await refresh({ forceRender: true, fresh: true });
          return;
        }
        await new Promise((resolve) => setTimeout(resolve, 400));
        await refresh({ fresh: true });
      }
      if (generation === state.sessionGeneration) throw new Error(t("ui.waitForRuntimeToTimeout"));
    } catch (error) {
      if (generation === state.sessionGeneration) toast(t("ui.couldNotOpenTheSystemBrowser"), error.message, 'error');
    } finally {
      state.openingBrowsers.delete(roomID);
    }
  }

  function resetConfirmState() {
    state.confirmRevision += 1;
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
    if (inputLabel) inputLabel.textContent = t("ui.enterTheFullIdToConfirm");
    if (input) {
      input.value = '';
      input.required = false;
      input.setCustomValidity('');
    }
    if (acknowledgementWrapper) acknowledgementWrapper.hidden = true;
    if (acknowledgementLabel) acknowledgementLabel.textContent = t("ui.iUnderstandThisActionCannotBeUndone");
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
    input.setCustomValidity(matches ? '' : t("ui.pleaseEnterACompleteWordForWordMatchingId"));
    acknowledgement.setCustomValidity(acknowledged ? '' : t("ui.pleaseMakeSureYouUnderstandThatThisOperationIsNotReversible"));
    submit.disabled = state.busyButtons.has(submit) || !matches || !acknowledged;
  }
  function openConfirm({ eyebrow = t('common.confirmUpper'), title, message, detail = '', label = t("ui.confirm"), tone = 'danger', confirmation = '', confirmationLabel = t("ui.enterTheFullIdToConfirm"), acknowledgement = '', action }) {
    resetConfirmState();
    state.confirmAction = action;
    state.confirmRequirement = confirmation;
    state.confirmAcknowledgementRequired = Boolean(acknowledgement);
    setRenderedText('confirm-eyebrow', eyebrow);
    setRenderedText('confirm-title', title);
    $('confirm-message').textContent = message;
    const detailNode = $('confirm-detail');
    detailNode.hidden = !detail;
    detailNode.replaceChildren();
    if (detail) detailNode.append(node('strong', { textContent: t("ui.securityBoundary") }), node('span', { textContent: detail }));
    const requirement = $('confirm-input-wrap');
    const input = $('confirm-input');
    const acknowledgementWrapper = $('confirm-ack-wrap');
    const acknowledgementInput = $('confirm-ack');
    requirement.hidden = !confirmation;
    setRenderedText('confirm-input-label', confirmationLabel);
    $('confirm-expected').textContent = confirmation;
    input.required = Boolean(confirmation);
    input.value = '';
    acknowledgementWrapper.hidden = !acknowledgement;
    setRenderedText('confirm-ack-label', acknowledgement || t("ui.iUnderstandThisActionCannotBeUndone"));
    acknowledgementInput.required = Boolean(acknowledgement);
    acknowledgementInput.checked = false;
    setRenderedText('confirm-submit', label);
    $('confirm-submit').className = tone === 'danger' ? 'danger-button' : 'primary-button';
    syncConfirmRequirement();
    showDialog('confirm-dialog');
    queueMicrotask(() => (confirmation ? input : (acknowledgement ? acknowledgementInput : $('confirm-submit'))).focus());
  }
  async function submitConfirm(event) {
    event.preventDefault();
    const action = state.confirmAction;
    const revision = state.confirmRevision;
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
        if (revision === state.confirmRevision && state.confirmAction === action) closeDialog('confirm-dialog');
      } catch (error) {
        toast(t("ui.actionFailed"), error.message, 'error');
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
	if (window.PairRoomTheme && window.PairRoomTheme.mode !== state.preferences.theme) window.PairRoomTheme.setTheme(state.preferences.theme);
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
    toast(t("ui.diagnosticsExported"), t("ui.runtimeUrlsAreDesensitizedLocalPathsMayStillBeSensitive"), 'success');
  }

  async function copyText(value, successMessage = t("ui.copied")) {
    try {
      await navigator.clipboard.writeText(String(value));
      toast(t("ui.copied324408e"), successMessage, 'success');
    } catch {
      const input = document.createElement('textarea');
      input.value = String(value);
      input.setAttribute('readonly', '');
      input.className = 'clipboard-copy-source';
      document.body.append(input);
      input.select();
      const copied = document.execCommand('copy');
      input.remove();
      toast(copied ? t("ui.copied324408e") : t("ui.copyFailed"), copied ? successMessage : t("ui.theBrowserRejectedTheClipboardOperation"), copied ? 'success' : 'error');
    }
  }

  function looksAbsolutePath(value) {
    return value.startsWith('/') || /^[A-Za-z]:[\\/]/.test(value) || /^\\\\[^\\]+\\[^\\]+/.test(value);
  }

  async function withBusy(button, work) {
    if (!button || button.disabled || state.busyButtons.has(button)) return;
    const original = button.textContent;
    state.busyButtons.add(button);
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
    const busyLabel = t("ui.processing");
    button.textContent = busyLabel;
    try { await work(); } finally {
      state.busyButtons.delete(button);
      button.disabled = false;
      button.setAttribute('aria-busy', 'false');
      if (button.textContent === busyLabel) button.textContent = original;
      if (button === $('confirm-submit')) syncConfirmRequirement();
    }
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
    return node('span', { className: `badge ${tone} ${extra}`.trim(), textContent: localizedStatus(label) });
  }

  function localizedStatus(value) {
	const labels = {
	  active: t('common.active'), available: t('common.available'), unavailable: t('common.unavailable'), archived: t('ui.archived'), legacy: t('common.legacy'),
	  queued: t('ui.queued'), starting: t('common.starting'), stopping: t('common.stopping'), suspended: t('common.suspended'), failed: t('ui.failed'),
	  full: t('common.full'), supported: t('common.supported'), 'not supported': t('common.notSupported'), healthy: t('common.healthy'), 'fail-closed': t('common.failClosed'),
	};
	return labels[value] || value;
  }

  function statCard(label, value, hint, symbol, tone = '', onClick = null) {
	const displayedValue = typeof value === 'number' ? formatNumber(value) : String(value);
    const card = node(onClick ? 'button' : 'article', { className: `panel stat-card ${tone}`.trim(), ...(onClick ? { type: 'button', onClick } : {}) },
      node('div', { className: 'stat-top' }, node('span', { className: 'stat-label', textContent: label }), node('span', { className: 'stat-icon', textContent: symbol, 'aria-hidden': 'true' })),
	  node('div', {}, node('div', { className: 'stat-value', textContent: displayedValue }), node('div', { className: 'stat-hint', textContent: hint }))
    );
	if (onClick) card.setAttribute('aria-label', `${label}: ${displayedValue}. ${hint}`);
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
	return node('div', { className: 'project-summary-item' }, node('strong', { textContent: typeof value === 'number' ? formatNumber(value) : String(value) }), node('span', { textContent: label }));
  }

  function keyValue(label, value, mono = false) {
    const content = value instanceof Node ? value : node(mono ? 'code' : 'strong', { textContent: value || '—' });
    return node('div', { className: 'key-value' }, node('span', { textContent: label }), content);
  }

  function githubRepositoryLink(url) {
    const href = String(url || GITHUB_REPOSITORY_URL).trim();
    if (href !== GITHUB_REPOSITORY_URL) return node('code', { textContent: href || '—' });
    return node('a', { href, target: '_blank', rel: 'noopener noreferrer', className: 'external-link', textContent: href, title: t('ui.openLink'), 'aria-label': t('ui.openLink') });
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
	return window.PairRoomI18n.formatDate(date, { dateStyle: 'medium', timeStyle: 'medium' });
  }

  function formatRelativeTime(value) {
    if (!value) return '—';
    const date = new Date(value);
    const time = date.getTime();
    if (Number.isNaN(time) || time <= 0) return '—';
    const delta = time - Date.now();
    if (!Number.isFinite(delta)) return String(value);
    const abs = Math.abs(delta);
    if (abs < 5000) return t("ui.just");
	if (abs < 60000) return window.PairRoomI18n.formatRelative(Math.round(delta / 1000), 'second');
	if (abs < 3600000) return window.PairRoomI18n.formatRelative(Math.round(delta / 60000), 'minute');
	if (abs < 86400000) return window.PairRoomI18n.formatRelative(Math.round(delta / 3600000), 'hour');
    return formatDateTime(value);
  }

  function formatDuration(seconds) {
    const value = Number(seconds);
    if (!Number.isFinite(value) || value <= 0) return '—';
	if (value % 3600 === 0) return t("ui.valueHours", { value0: formatNumber(value / 3600) });
	if (value % 60 === 0) return t("ui.valueMinutes", { value0: formatNumber(value / 60) });
	return t("ui.valueSeconds", { value0: formatNumber(value) });
  }

  function formatNumber(value, options) {
	return window.PairRoomI18n.formatNumber(Number(value || 0), options);
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
      if (dialog.id === 'room-dialog') state.roomDialogRevision += 1;
      if (state.renderPending && !document.querySelector('dialog[open]')) {
        render();
        state.renderPending = false;
      }
    });
  });
  document.querySelectorAll('[data-project-mode]').forEach((button) => button.addEventListener('click', () => setProjectMode(button.dataset.projectMode)));
  document.querySelectorAll('input[name$="-mode"]').forEach((input) => input.addEventListener('change', syncBindingInputs));
  for (const actor of ['claude', 'codex']) {
    $(`${actor}-runtime`).addEventListener('change', () => {
      const entry = runtimeCatalogEntry($(`${actor}-runtime`).value);
      $(`${actor}-runtime`).setCustomValidity(entry?.available ? '' : t('agent.unavailable'));
      const diagnostic = $(`${actor}-runtime-diagnostic`);
      diagnostic.textContent = entry?.diagnostic || (entry?.version ? `v${entry.version}` : '');
      diagnostic.classList.toggle('runtime-unavailable', !entry?.available);
      syncAgentProviderAndModels(actor, true);
    });
    $(`${actor}-provider`).addEventListener('change', () => {
      syncAgentProviderAndModels(actor, false);
      $(`${actor}-model`).value = '';
    });
  }
  $('agent-catalog-refresh').addEventListener('click', async () => {
    const revision = state.roomDialogRevision;
    const wasReady = state.pairProfileReady;
    await withBusy($('agent-catalog-refresh'), async () => {
      try {
        const [catalog, profiles] = await Promise.all([loadAgentCatalog(true), wasReady ? null : loadAgentPairProfiles(true)]);
        if (revision !== state.roomDialogRevision || !$('room-dialog').open || !state.authenticated) return;
        if (wasReady) {
          const current = { claude: readAgentSelection('claude'), codex: readAgentSelection('codex') };
          for (const actor of ['claude', 'codex']) populateAgentControls(actor, current[actor] || catalog.defaults?.[actor]);
        } else {
          const id = state.pairProfileMode ? state.pairProfileID : (profiles.default_profile_id || '');
          populatePairProfilePicker(id);
          applyPairProfile(id);
          state.pairProfileReady = true;
          $('room-pair-profile').disabled = false;
          $('pair-profile-save-new').disabled = false;
          $('room-submit').disabled = false;
        }
        syncCollaborationControls();
        hideFormError('room-form-error');
        toast(t('agent.catalogRefreshed'), '', 'success');
      } catch (error) {
        showFormError('room-form-error', error.message);
      }
    });
  });
  $('room-pair-profile').addEventListener('change', () => {
    try { applyPairProfile($('room-pair-profile').value); hideFormError('room-form-error'); }
    catch (error) { showFormError('room-form-error', error.message); }
  });
  $('pair-profile-save-new').addEventListener('click', () => saveCurrentPairProfile(false));
  $('pair-profile-update').addEventListener('click', () => saveCurrentPairProfile(true));
  $('room-project-id').addEventListener('change', () => {
    const project = state.snapshot?.projects?.find((item) => item.id === $('room-project-id').value);
    setRenderedText('room-dialog-title', t("ui.createRoomInValue", { value0: (projectName(project)) }));
  });
  $('project-form').addEventListener('submit', submitProject);
  $('room-collaboration-mode').addEventListener('change', syncCollaborationControls);
  $('room-form').addEventListener('submit', createRoom);
  $('rename-form').addEventListener('submit', submitRename);
  document.addEventListener('contextmenu', (event) => openRoomContextMenu(event));
  document.addEventListener('keydown', (event) => {
    if (event.key === 'ContextMenu' || (event.shiftKey && event.key === 'F10')) {
      openRoomContextMenu(event, true);
    } else if (state.contextRoomID && event.key === 'Escape') {
      event.preventDefault(); closeRoomContextMenu(true);
    } else if (state.contextRoomID && ['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
      event.preventDefault(); $('context-rename-room').focus();
    } else if (state.contextRoomID && event.key === 'Tab') {
      closeRoomContextMenu(true);
    }
  });
  $('context-rename-room').addEventListener('click', () => {
    const room = roomByID(state.contextRoomID);
    closeRoomContextMenu(true);
    if (room && state.authenticated) openRenameDialog(room);
  });
  document.addEventListener('pointerdown', (event) => {
    if (state.contextRoomID && !$('room-context-menu').contains(event.target)) closeRoomContextMenu();
  }, true);
  document.addEventListener('scroll', (event) => {
    if (!state.contextRoomID) return;
    const target = event.target === document ? document.scrollingElement : event.target;
    const openedAt = state.contextScrollPositions.get(target);
    // A focus/click scroll may already have happened before contextmenu while
    // its notification is still queued. Only a subsequent ancestor movement
    // invalidates the menu's viewport position; unrelated panels do not.
    if (openedAt && (target.scrollLeft !== openedAt[0] || target.scrollTop !== openedAt[1])) closeRoomContextMenu();
  }, true);
  window.addEventListener('resize', () => closeRoomContextMenu());
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
    diagnostics.cancel();
    ordering.cancel();
    closeRoomContextMenu();
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
    const frame = frames.find((frame) => frame.contentWindow === event.source);
    const roomID = frame?.parentElement?.dataset.roomId;
    if (!roomID || (data.roomId && data.roomId !== roomID)) return;
    if (data.action === 'close-tab') {
      closeTab(roomID);
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
    scheduleRefresh();
    if (state.authenticated && !document.hidden) refresh({ forceRender: state.renderPending });
  });

  function loadStoredTheme() {
    let theme = '';
    try { theme = String(window.localStorage.getItem('pairroom.theme') || ''); } catch { theme = ''; }
	if (theme === 'system' || theme === 'light' || theme === 'dark') state.preferences.theme = theme;
  }

  function persistTheme(value) {
	if (window.PairRoomTheme) window.PairRoomTheme.setTheme(value);
  }

  loadStoredTheme();
  window.addEventListener('storage', (event) => {
    if (event.key !== 'pairroom.theme') return;
    loadStoredTheme();
    applyPreferences();
  });
	document.addEventListener('pairroom:theme', (event) => {
	  const next = event.detail?.mode;
	  if (!['system', 'light', 'dark'].includes(next) || state.preferences.theme === next) return;
	  state.preferences.theme = next;
	  if (state.route.name === 'settings') renderSettings();
	});

  applyPreferences();
  document.addEventListener('pairroom:lang', () => {
    closeRoomContextMenu();
    if (window.PairRoomI18n) window.PairRoomI18n.apply(document);
    render();
    if ($('room-dialog').open) {
      syncCollaborationControls();
      syncPairProfileDialogLabels();
      if (state.pairProfileReady && !state.pairProfileBusy) populatePairProfilePicker(state.pairProfileID);
    }
  });
  scheduleRefresh();
  renderLoading();
  connect({ forceRender: true });
})();
