(() => {
  'use strict';

  const INITIAL_ROUTE = '#/overview';
  const NEW_BINDING_HINT = 'materializes on first turn';
  const hashParams = new URLSearchParams(location.hash.replace(/^#/, ''));
  let token = hashParams.get('token') || '';
  if (token) {
    history.replaceState(null, '', `${location.pathname}${location.search}${INITIAL_ROUTE}`);
  } else if (!location.hash.startsWith('#/')) {
    history.replaceState(null, '', `${location.pathname}${location.search}${INITIAL_ROUTE}`);
  }

  const state = {
    snapshot: null,
    route: parseRoute(),
    connected: false,
    lastError: '',
    search: '',
    refreshPromise: null,
    refreshTimer: null,
    renderPending: false,
    opening: new Map(),
    pendingNavigationRoom: '',
    projectMode: 'register',
    bindingRoomID: '',
    confirmAction: null,
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
      openBehavior: 'new',
    },
  };

  const $ = (id) => document.getElementById(id);
  const app = $('app');
  const view = $('view');

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    headers.set('Authorization', `Bearer ${token}`);
    if (options.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
    const response = await fetch(path, { ...options, headers, credentials: 'same-origin' });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(payload.error || response.statusText || `HTTP ${response.status}`);
    return payload;
  }

  async function refresh({ notify = false, forceRender = false } = {}) {
    if (!token) {
      setDisconnected('请使用 PairRoom Service 启动输出中的完整 Management Shell 地址。');
      renderMissingToken();
      return null;
    }
    if (state.refreshPromise) return state.refreshPromise;
    $('refresh-button').classList.add('spinning');
    state.refreshPromise = api('/api/v1/service').then((snapshot) => {
      state.snapshot = snapshot;
      state.connected = true;
      state.lastError = '';
      updateChrome();
      if (forceRender || canRenderNow()) {
        render();
        state.renderPending = false;
      } else {
        state.renderPending = true;
      }
      resolveOpeningRooms();
      if (notify) toast('已同步', 'Management Shell 状态已刷新。', 'success');
      return snapshot;
    }).catch((error) => {
      const changed = state.connected || state.lastError !== error.message;
      state.connected = false;
      state.lastError = error.message;
      setDisconnected(error.message);
      if (notify || changed) toast('连接失败', error.message, 'error');
      return null;
    }).finally(() => {
      state.refreshPromise = null;
      $('refresh-button').classList.remove('spinning');
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
        if (!document.hidden) refresh();
      }, state.preferences.refreshMs);
    }
  }

  function parseRoute() {
    const raw = location.hash.startsWith('#/') ? location.hash.slice(2) : 'overview';
    const parts = raw.split('/').filter(Boolean).map((part) => {
      try { return decodeURIComponent(part); } catch { return part; }
    });
    if (parts[0] === 'projects' && parts[1]) return { name: 'project', projectID: parts[1] };
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
      const current = state.route.name === 'project' ? 'projects' : state.route.name;
      node.classList.toggle('active', node.dataset.nav === current);
      if (node.dataset.nav === current) node.setAttribute('aria-current', 'page');
      else node.removeAttribute('aria-current');
    });
    if (snapshot?.generated_at) $('last-updated').textContent = `最近同步 ${formatRelativeTime(snapshot.generated_at)}`;
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
      default:
        return { eyebrow: 'PAIRROOM SERVICE', title: '概览', subtitle: 'Claude Code 与 Codex 的多 Project、本地协作控制面。' };
    }
  }

  function render() {
    state.route = parseRoute();
    updateChrome();
    if (!state.snapshot) {
      renderLoading();
      return;
    }
    switch (state.route.name) {
      case 'projects': renderProjects(); break;
      case 'project': renderProjectDetail(state.route.projectID); break;
      case 'runtimes': renderRuntimes(); break;
      case 'settings': renderSettings(); break;
      default: renderOverview(); break;
    }
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

  function renderMissingToken() {
    view.replaceChildren(
      emptyState('!', '缺少 Management Token', '请从 PairRoom Service 启动输出中打开完整地址。Token 只在初始 URL fragment 中读取并立即从地址栏移除。')
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
          statCard('需要关注', summary.attention_items, summary.attention_items ? 'Binding、Runtime 或路径诊断' : '当前无阻断项', '!', summary.attention_items ? 'danger' : 'good')
        ),
        node('section', { className: 'two-panel-grid' },
          panel('需要关注', '阻断激活或需要人工处理的项目',
            attention.length ? node('div', { className: 'list' }, ...attention.slice(0, 8).map(renderAttentionItem))
              : emptyState('✓', '一切正常', '当前没有不可用 Project、失败 Runtime 或待补全 Binding。', true),
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
    return node('article', { className: 'panel project-card' },
      node('header', { className: 'project-card-header' },
        node('div', { className: 'project-avatar', textContent: projectInitials(project) }),
        node('div', { className: 'project-card-title' },
          node('div', {}, node('h2', { textContent: projectName(project) }), statusBadge(project.available ? 'available' : 'unavailable', project.available ? 'good' : 'danger')),
          node('code', { className: 'project-path', textContent: project.root, title: project.root })
        ),
        node('div', { className: 'project-card-actions' },
          actionButton('复制路径', () => copyText(project.root, 'Project 路径已复制。'), 'secondary-button compact-button'),
          actionButton('详情', () => navigate(`#/projects/${encodeURIComponent(project.id)}`), 'secondary-button compact-button'),
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
        : emptyState('◇', state.filters.showArchived ? '此 Project 尚无 Room' : '此 Project 尚无活动 Room', project.available ? '创建一个 Room，绑定 Claude 与 Codex。' : '修复 Project 路径后才能创建 Room。', true,
          project.available ? actionButton('创建 Room', () => openRoomDialog(project.id), 'secondary-button compact-button') : null)
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
    if (archived) {
      actions.append(actionButton('恢复', () => restoreRoom(room), 'secondary-button'));
    } else {
      if (pending) actions.append(actionButton('补全 Binding', () => completeBindings(room), 'primary-button'));
      actions.append(actionButton(runtime.phase === 'queued' ? `排队 #${runtime.queue_position || '?'}` : '打开', () => openRoom(room.id), 'primary-button', pending));
      actions.append(actionButton('重命名', () => openRenameDialog(room), 'secondary-button'));
      actions.append(actionButton('归档', () => archiveRoom(room), 'danger-button outline'));
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
    const nav = node('nav', { className: 'panel settings-nav', 'aria-label': '设置分区' }, ...sections.map(([key, label]) => actionButton(label, () => { state.settingsSection = key; renderSettings(); }, state.settingsSection === key ? 'active' : '')));
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
          settingRow('最大活动 Runtime', 'Starting、Active、Stopping，以及清理不确定的 Failed Runtime 都占用容量。', node('strong', { textContent: policy.limit ? String(policy.limit) : '未暴露' })),
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
        node('aside', { className: 'callout warning' }, node('strong', { textContent: '为何只读' }), node('span', { textContent: '降低容量不会中断 Busy Runtime；PairRoom 选择显式重启边界，而不是提供看似即时、实则可能破坏 Session 唯一性的热更新。' }))
      );
    }
    if (state.settingsSection === 'operations') {
      const daemonInstall = daemonInstallCommand(policy);
      return node('div', { className: 'view-stack' },
        settingsPanel('Daemon 快捷命令', 'PairRoom Web Shell 不直接停止或重启承载自身的宿主进程；运维动作在本机终端执行。',
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
          capabilityRow('热更新 Runtime policy', caps.runtime_policy_mutation === true, '当前需受控重启。'),
          capabilityRow('永久删除 Room', caps.room_deletion === true, '归档是唯一隐藏操作，历史与 Binding Identity 保留。'),
          capabilityRow('服务端路径浏览器', caps.server_path_browser === true, '避免扩大本机文件系统暴露面。')
        ),
        node('aside', { className: 'callout boundary' }, node('strong', { textContent: 'Transcript Boundary' }), node('span', { textContent: 'Vendor Transcript 与 PairRoom Room Event Log 是不同记录。Existing Binding 只恢复 vendor context，绑定前历史不会进入公共时间线。' })),
        node('aside', { className: 'callout warning' }, node('strong', { textContent: 'Binding Identity' }), node('span', { textContent: '(agent, vendor_session_id) 在整个 Service 内独占，包括已归档 Room。该约束防止同一 vendor transcript 被多个 Runtime 并发写入。' })),
        node('aside', { className: 'callout neutral' }, node('strong', { textContent: 'Browser token' }), node('span', { textContent: 'Management token 只从启动 URL fragment 读取并立即从地址栏移除；API 请求使用内存中的 Bearer 值。' }))
      );
    }
    return node('div', { className: 'view-stack' },
      settingsPanel('外观', '偏好只在当前 Management Shell 标签页生效。',
        settingRow('主题', '跟随系统，或临时固定浅色/深色。', segmented([
          ['system', '跟随系统'], ['light', '浅色'], ['dark', '深色'],
        ], state.preferences.theme, (value) => { state.preferences.theme = value; applyPreferences(); renderSettings(); })),
        settingRow('信息密度', 'Compact 会缩小列表、表格和面板间距。', segmented([
          ['comfortable', '舒适'], ['compact', '紧凑'],
        ], state.preferences.density, (value) => { state.preferences.density = value; applyPreferences(); renderSettings(); }))
      ),
      settingsPanel('刷新与导航', '控制当前页面如何轮询 Service 与打开 Room。',
        settingRow('自动刷新', '页面隐藏时自动暂停，重新可见后立即同步。', selectControl([
          ['0', '关闭'], ['5000', '5 秒'], ['10000', '10 秒'], ['30000', '30 秒'], ['60000', '60 秒'],
        ], String(state.preferences.refreshMs), (value) => { state.preferences.refreshMs = Number(value); scheduleRefresh(); }, '自动刷新间隔')),
        settingRow('Room 打开方式', '新标签页可以保留 Management Shell；同标签页仅在 Runtime 已可用后跳转。', segmented([
          ['new', '新标签页'], ['same', '当前标签页'],
        ], state.preferences.openBehavior, (value) => { state.preferences.openBehavior = value; renderSettings(); })),
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
    (snapshot.runtimes || []).forEach((runtime) => {
      if (runtime.occupies_capacity || ['active', 'starting', 'stopping'].includes(runtime.phase)) summary.runtime_capacity_used++;
      if (runtime.phase === 'active') summary.active_runtimes++;
      if (runtime.busy) summary.busy_runtimes++;
      if (runtime.phase === 'queued') summary.queued_runtimes++;
      if (runtime.phase === 'failed') summary.failed_runtimes++;
    });
    summary.attention_items = summary.unavailable_projects + summary.pending_bindings + summary.failed_runtimes;
    return summary;
  }

  function emptySummary() {
    return { projects: 0, unavailable_projects: 0, rooms: 0, active_rooms: 0, archived_rooms: 0, pending_bindings: 0, runtime_capacity_used: 0, active_runtimes: 0, busy_runtimes: 0, queued_runtimes: 0, failed_runtimes: 0, attention_items: 0 };
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
      message: '活动 Turn 会先自然完成，Runtime 随后挂起；Room 将从默认列表隐藏。',
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
    if (!room || room.lifecycle === 'archived') return;
    if (roomHasBlockingPendingBindings(room)) {
      completeBindings(room);
      return;
    }
    let popup = null;
    const newWindow = state.preferences.openBehavior === 'new';
    if (newWindow) {
      popup = state.opening.get(roomID);
      if (!popup || popup.closed) popup = window.open('about:blank', '_blank');
      if (!popup) {
        toast('弹窗被阻止', '允许此站点打开新标签页后重试，或在设置中改为当前标签页。', 'error');
        return;
      }
      renderActivationPlaceholder(popup, room.name);
      state.opening.set(roomID, popup);
    } else {
      state.pendingNavigationRoom = roomID;
    }
    try {
      const status = await api(`/api/v1/rooms/${encodeURIComponent(roomID)}/activate`, { method: 'POST' });
      if (status.url) {
        if (popup) popup.location.replace(status.url);
        else location.assign(status.url);
        state.opening.delete(roomID);
        state.pendingNavigationRoom = '';
      } else {
        toast(status.queue_position ? `Room 已进入队列 #${status.queue_position}` : 'Runtime 正在启动', '关闭 Management Shell 或激活占位页都不会取消 durable activation demand。', 'success');
      }
      await refresh({ forceRender: true });
    } catch (error) {
      if (popup && !popup.closed) popup.close();
      state.opening.delete(roomID);
      if (state.pendingNavigationRoom === roomID) state.pendingNavigationRoom = '';
      toast('Room 激活失败', error.message, 'error');
    }
  }

  function renderActivationPlaceholder(popup, roomName) {
    try {
      popup.document.title = `${roomName} · PairRoom activating`;
      popup.document.body.replaceChildren();
      popup.document.body.style.cssText = 'margin:0;min-height:100vh;display:grid;place-items:center;background:#0b1020;color:#e8eefb;font:14px system-ui,sans-serif';
      const box = popup.document.createElement('div');
      box.style.cssText = 'max-width:520px;padding:28px;text-align:center';
      const title = popup.document.createElement('h1');
      title.textContent = `正在激活 ${roomName}`;
      title.style.cssText = 'font-size:22px;margin:0 0 10px';
      const copy = popup.document.createElement('p');
      copy.textContent = 'Runtime 正在启动或等待全局容量。关闭此页不会中断其他 Room 的活动 Turn。';
      copy.style.cssText = 'color:#9eabc1;line-height:1.6';
      box.append(title, copy);
      popup.document.body.append(box);
    } catch {
      // The placeholder is best-effort; activation itself remains authoritative.
    }
  }

  function resolveOpeningRooms() {
    const runtimeByRoom = new Map((state.snapshot?.runtimes || []).map((runtime) => [runtime.room_id, runtime]));
    for (const [roomID, popup] of state.opening) {
      const runtime = runtimeByRoom.get(roomID);
      if (!popup || popup.closed) {
        state.opening.delete(roomID);
        continue;
      }
      if (runtime?.phase === 'active' && runtime.url) {
        popup.location.replace(runtime.url);
        state.opening.delete(roomID);
      } else if (runtime?.phase === 'failed') {
        popup.close();
        state.opening.delete(roomID);
        toast('Runtime 启动失败', runtime.last_error || '请查看 Runtimes 页面诊断。', 'error');
      }
    }
    if (state.pendingNavigationRoom) {
      const runtime = runtimeByRoom.get(state.pendingNavigationRoom);
      if (runtime?.phase === 'active' && runtime.url) {
        state.pendingNavigationRoom = '';
        location.assign(runtime.url);
      } else if (runtime?.phase === 'failed') {
        state.pendingNavigationRoom = '';
        toast('Runtime 启动失败', runtime.last_error || '请查看 Runtimes 页面诊断。', 'error');
      }
    }
  }

  function openConfirm({ eyebrow = 'CONFIRM', title, message, detail = '', label = '确认', tone = 'danger', action }) {
    state.confirmAction = action;
    $('confirm-eyebrow').textContent = eyebrow;
    $('confirm-title').textContent = title;
    $('confirm-message').textContent = message;
    const detailNode = $('confirm-detail');
    detailNode.hidden = !detail;
    detailNode.replaceChildren();
    if (detail) detailNode.append(node('strong', { textContent: '安全边界' }), node('span', { textContent: detail }));
    $('confirm-submit').textContent = label;
    $('confirm-submit').className = tone === 'danger' ? 'danger-button' : 'primary-button';
    showDialog('confirm-dialog');
  }

  async function submitConfirm(event) {
    event.preventDefault();
    const action = state.confirmAction;
    if (!action) {
      closeDialog('confirm-dialog');
      return;
    }
    await withBusy($('confirm-submit'), async () => {
      try {
        await action();
        closeDialog('confirm-dialog');
        state.confirmAction = null;
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

  function segmented(options, value, onChange) {
    return node('div', { className: 'segmented-control', role: 'group' }, ...options.map(([optionValue, label]) => actionButton(label, () => onChange(optionValue), value === optionValue ? 'active' : '')));
  }

  function toggleButton(value, onChange, label) {
    return node('button', { type: 'button', className: 'toggle-switch', role: 'switch', 'aria-checked': String(value), 'aria-label': label, onClick: () => { onChange(!value); if (state.route.name === 'settings') renderSettings(); } });
  }

  function formatDateTime(value) {
    if (!value) return '—';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'medium' }).format(date);
  }

  function formatRelativeTime(value) {
    if (!value) return '—';
    const date = new Date(value);
    const delta = Date.now() - date.getTime();
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

  document.querySelectorAll('[data-close-dialog]').forEach((button) => button.addEventListener('click', () => closeDialog(button.dataset.closeDialog)));
  document.querySelectorAll('dialog').forEach((dialog) => {
    dialog.addEventListener('click', (event) => { if (event.target === dialog) closeDialog(dialog.id); });
    dialog.addEventListener('cancel', () => { if (dialog.id === 'confirm-dialog') state.confirmAction = null; });
    dialog.addEventListener('close', () => {
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
  $('refresh-button').addEventListener('click', () => refresh({ notify: true, forceRender: true }));
  $('retry-button').addEventListener('click', () => refresh({ notify: true, forceRender: true }));
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
    state.route = parseRoute();
    app.classList.remove('sidebar-open');
    render();
    view.focus({ preventScroll: true });
    window.scrollTo({ top: 0, behavior: 'instant' });
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === '/' && !event.metaKey && !event.ctrlKey && !event.altKey && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement?.tagName)) {
      event.preventDefault();
      $('global-search').focus();
    }
  });
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) refresh({ forceRender: state.renderPending });
  });

  applyPreferences();
  scheduleRefresh();
  renderLoading();
  refresh({ forceRender: true });
})();
