(() => {
  'use strict';
  const hash = new URLSearchParams(location.hash.replace(/^#/, ''));
  let token = hash.get('token') || '';
  if (token) history.replaceState(null, '', `${location.pathname}${location.search}`);

  const state = { snapshot: null, showArchived: false, opening: new Map(), refreshPromise: null };
  const $ = (id) => document.getElementById(id);

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    headers.set('Authorization', `Bearer ${token}`);
    if (options.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
    const response = await fetch(path, { ...options, headers, credentials: 'same-origin' });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(payload.error || response.statusText);
    return payload;
  }

  async function refresh() {
    if (state.refreshPromise) return state.refreshPromise;
    state.refreshPromise = api('/api/v1/service').then((snapshot) => {
      state.snapshot = snapshot;
      render();
      resolveOpeningRooms();
    }).catch((error) => {
      $('health').textContent = 'Disconnected';
      $('health').className = 'badge danger';
      toast(error.message, 'error');
    }).finally(() => { state.refreshPromise = null; });
    return state.refreshPromise;
  }

  function render() {
    const snapshot = state.snapshot;
    $('service-meta').textContent = `PairRoom ${snapshot.version} · ${snapshot.data_root}`;
    $('health').textContent = snapshot.healthy ? 'Healthy' : 'Fail-closed';
    $('health').className = `badge ${snapshot.healthy ? 'good' : 'danger'}`;
    if (snapshot.diagnostic) $('health').title = snapshot.diagnostic;

    const runtimeByRoom = new Map((snapshot.runtimes || []).map((item) => [item.room_id, item]));
    const roomsByProject = new Map();
    for (const room of snapshot.rooms || []) {
      if (!state.showArchived && room.lifecycle === 'archived') continue;
      const values = roomsByProject.get(room.project_id) || [];
      values.push(room);
      roomsByProject.set(room.project_id, values);
    }
    const container = $('projects');
    container.replaceChildren();
    if (!(snapshot.projects || []).length) {
      container.append(el('div', { className: 'panel empty', textContent: '尚未登记 Project。请显式输入一个 Git worktree 中的绝对路径。' }));
      return;
    }
    for (const project of snapshot.projects) {
      const card = el('section', { className: 'project' });
      const header = el('div', { className: 'project-header' });
      const summary = el('div');
      const title = el('div', { className: 'project-title' });
      title.append(el('strong', { textContent: project.root.split(/[\\/]/).filter(Boolean).pop() || project.root }));
      title.append(statusBadge(project.available ? 'available' : 'unavailable', project.available ? 'good' : 'danger'));
      summary.append(title, el('code', { textContent: project.root }));
      if (project.diagnostic) summary.append(el('p', { className: 'hint', textContent: project.diagnostic }));
      const create = el('button', { textContent: '创建 Room', disabled: !project.available });
      create.addEventListener('click', () => openCreateDialog(project));
      header.append(summary, create);
      card.append(header);

      const rooms = el('div', { className: 'rooms' });
      const values = roomsByProject.get(project.id) || [];
      if (!values.length) rooms.append(el('div', { className: 'empty', textContent: state.showArchived ? '此 Project 尚无 Room。' : '此 Project 尚无活动 Room。' }));
      for (const room of values) rooms.append(roomRow(room, runtimeByRoom.get(room.id) || { phase: 'suspended' }));
      card.append(rooms);
      container.append(card);
    }
  }

  function roomRow(room, runtime) {
    const row = el('article', { className: 'room-row' });
    const main = el('div');
    const title = el('div', { className: 'room-title' });
    title.append(el('strong', { textContent: room.name }));
    title.append(statusBadge(room.lifecycle, room.lifecycle === 'archived' ? 'warn' : 'good'));
    title.append(statusBadge(runtimeText(runtime), runtime.phase === 'failed' ? 'danger' : runtime.busy ? 'warn' : ''));
    if (room.legacy) title.append(statusBadge('legacy', ''));
    const meta = el('div', { className: 'room-meta' });
    meta.append(el('span', { textContent: `Claude: ${bindingText(room.bindings?.claude)}` }));
    meta.append(el('span', { textContent: `Codex: ${bindingText(room.bindings?.codex)}` }));
    meta.append(el('code', { textContent: room.id }));
    main.append(title, meta);
    if (runtime.last_error) main.append(el('p', { className: 'hint', textContent: runtime.last_error }));

    const actions = el('div', { className: 'actions' });
    if (room.lifecycle !== 'archived') {
      const pending = roomHasPendingBindings(room);
      const open = el('button', { textContent: pending ? '先补全 Binding' : (runtime.phase === 'queued' ? `排队 #${runtime.queue_position || '?'}` : '打开'), disabled: pending });
      open.addEventListener('click', () => openRoom(room.id));
      if (pending) {
        const complete = el('button', { textContent: '补全 Binding' });
        complete.addEventListener('click', () => completeBindings(room));
        actions.append(complete);
      }
      const rename = el('button', { className: 'secondary', textContent: '重命名' });
      rename.addEventListener('click', () => renameRoom(room));
      const archive = el('button', { className: 'danger', textContent: '归档' });
      archive.addEventListener('click', () => archiveRoom(room));
      actions.append(open, rename, archive);
    } else {
      const restore = el('button', { className: 'secondary', textContent: '恢复' });
      restore.addEventListener('click', () => restoreRoom(room));
      actions.append(restore);
    }
    row.append(main, actions);
    return row;
  }

  function runtimeText(runtime) {
    if (runtime.phase === 'queued') return `queued #${runtime.queue_position || '?'}`;
    if (runtime.phase === 'active' && runtime.busy) return 'active · working';
    return runtime.phase || 'suspended';
  }

  function bindingText(binding) {
    if (!binding) return 'missing';
    if (binding.pending) return 'pending legacy binding';
    const id = String(binding.session_id || '');
    return `${binding.mode} · ${id.length > 28 ? `${id.slice(0, 13)}…${id.slice(-10)}` : id}`;
  }

  function roomHasPendingBindings(room) {
    return ['claude', 'codex'].some((actor) => !room.bindings?.[actor] || room.bindings[actor].pending);
  }

  async function completeBindings(room) {
    const bindings = {};
    for (const actor of ['claude', 'codex']) {
      if (room.bindings?.[actor] && !room.bindings[actor].pending) continue;
      const choice = prompt(`${actor === 'claude' ? 'Claude' : 'Codex'} Binding：输入 new 新建，或粘贴已有 Session/Thread ID`, 'new');
      if (choice === null) return;
      const value = choice.trim();
      if (!value) { toast('Binding 不能为空。', 'error'); return; }
      bindings[actor] = value.toLowerCase() === 'new' ? { mode: 'new' } : { mode: 'existing', session_id: value };
    }
    try {
      await api(`/api/v1/rooms/${encodeURIComponent(room.id)}/bindings`, { method: 'POST', body: JSON.stringify({ bindings }) });
      toast('Legacy Room Binding 已原子补全。', 'success');
      await refresh();
    } catch (error) { toast(error.message, 'error'); }
  }

  async function openRoom(roomID) {
    let popup = state.opening.get(roomID);
    if (!popup || popup.closed) popup = window.open('about:blank', '_blank');
    if (!popup) { toast('浏览器阻止了新窗口；允许此站点弹窗后重试。', 'error'); return; }
    popup.document.title = 'PairRoom is activating…';
    popup.document.body.textContent = 'PairRoom Runtime 正在激活或排队；关闭此窗口不会中断其他 Room。';
    state.opening.set(roomID, popup);
    try {
      const status = await api(`/api/v1/rooms/${encodeURIComponent(roomID)}/activate`, { method: 'POST' });
      if (status.url) {
        popup.location.replace(status.url);
        state.opening.delete(roomID);
      } else {
        toast(status.queue_position ? `Room 已进入运行队列，第 ${status.queue_position} 位。` : 'Room Runtime 正在启动。', 'success');
      }
      refresh();
    } catch (error) {
      popup.close();
      state.opening.delete(roomID);
      toast(error.message, 'error');
    }
  }

  function resolveOpeningRooms() {
    const runtimeByRoom = new Map((state.snapshot?.runtimes || []).map((item) => [item.room_id, item]));
    for (const [roomID, popup] of state.opening) {
      const runtime = runtimeByRoom.get(roomID);
      if (!popup || popup.closed) { state.opening.delete(roomID); continue; }
      if (runtime?.phase === 'active' && runtime.url) {
        popup.location.replace(runtime.url);
        state.opening.delete(roomID);
      } else if (runtime?.phase === 'failed') {
        popup.close();
        state.opening.delete(roomID);
        toast(runtime.last_error || 'Room Runtime 启动失败。', 'error');
      }
    }
  }

  function openCreateDialog(project) {
    $('room-project-id').value = project.id;
    $('room-dialog-title').textContent = `在 ${project.root.split(/[\\/]/).filter(Boolean).pop() || project.root} 中创建 Room`;
    $('room-name').value = '';
    document.querySelector('input[name=claude-mode][value=new]').checked = true;
    document.querySelector('input[name=codex-mode][value=new]').checked = true;
    $('claude-session-id').value = '';
    $('codex-session-id').value = '';
    syncBindingInputs();
    $('room-dialog').showModal();
    $('room-name').focus();
  }

  async function createRoom(event) {
    event.preventDefault();
    const projectID = $('room-project-id').value;
    const claudeMode = document.querySelector('input[name=claude-mode]:checked').value;
    const codexMode = document.querySelector('input[name=codex-mode]:checked').value;
    const payload = {
      name: $('room-name').value,
      bindings: {
        claude: { mode: claudeMode, ...(claudeMode === 'existing' ? { session_id: $('claude-session-id').value } : {}) },
        codex: { mode: codexMode, ...(codexMode === 'existing' ? { session_id: $('codex-session-id').value } : {}) },
      },
    };
    try {
      await api(`/api/v1/projects/${encodeURIComponent(projectID)}/rooms`, { method: 'POST', body: JSON.stringify(payload) });
      $('room-dialog').close();
      toast('Room 已原子创建。', 'success');
      await refresh();
    } catch (error) { toast(error.message, 'error'); }
  }

  async function renameRoom(room) {
    const name = prompt('新的 Room 名称', room.name);
    if (name === null || name.trim() === room.name) return;
    try {
      await api(`/api/v1/rooms/${encodeURIComponent(room.id)}`, { method: 'PATCH', body: JSON.stringify({ name }) });
      toast('Room 已在安全 Turn 边界重命名。', 'success');
      await refresh();
    } catch (error) { toast(error.message, 'error'); }
  }

  async function archiveRoom(room) {
    if (!confirm(`归档 “${room.name}”？活动 Turn 会先自然完成，历史、附件和 Binding Identity 将完整保留。`)) return;
    try {
      await api(`/api/v1/rooms/${encodeURIComponent(room.id)}/archive`, { method: 'POST' });
      toast('Room 已归档。', 'success');
      await refresh();
    } catch (error) { toast(error.message, 'error'); }
  }

  async function restoreRoom(room) {
    try {
      await api(`/api/v1/rooms/${encodeURIComponent(room.id)}/restore`, { method: 'POST' });
      toast('Room 已恢复。', 'success');
      await refresh();
    } catch (error) { toast(error.message, 'error'); }
  }

  function syncBindingInputs() {
    $('claude-session-id').disabled = document.querySelector('input[name=claude-mode]:checked').value !== 'existing';
    $('codex-session-id').disabled = document.querySelector('input[name=codex-mode]:checked').value !== 'existing';
    $('claude-session-id').required = !$('claude-session-id').disabled;
    $('codex-session-id').required = !$('codex-session-id').disabled;
  }

  function statusBadge(text, tone) { return el('span', { className: `badge ${tone || ''}`, textContent: text }); }
  function el(tag, properties = {}) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(properties)) {
      if (key === 'className') node.className = value;
      else if (key === 'textContent') node.textContent = value;
      else if (key === 'disabled') node.disabled = Boolean(value);
      else node.setAttribute(key, value);
    }
    return node;
  }
  function toast(message, type = '') {
    const node = el('div', { className: `toast ${type}`, textContent: message });
    $('toasts').append(node);
    setTimeout(() => node.remove(), 5000);
  }

  $('project-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    try {
      await api('/api/v1/projects', { method: 'POST', body: JSON.stringify({ path: $('project-path').value }) });
      $('project-path').value = '';
      toast('Project 已登记。', 'success');
      await refresh();
    } catch (error) { toast(error.message, 'error'); }
  });
  $('import-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    try {
      await api('/api/v1/import', { method: 'POST', body: JSON.stringify({ path: $('import-path').value }) });
      $('import-path').value = '';
      toast('旧 Room 已只读登记。', 'success');
      await refresh();
    } catch (error) { toast(error.message, 'error'); }
  });
  $('room-form').addEventListener('submit', createRoom);
  $('room-dialog-close').addEventListener('click', () => $('room-dialog').close());
  document.querySelectorAll('input[name$="-mode"]').forEach((input) => input.addEventListener('change', syncBindingInputs));
  $('show-archived').addEventListener('change', (event) => { state.showArchived = event.target.checked; render(); });

  if (!token) {
    $('health').textContent = 'Missing token';
    $('health').className = 'badge danger';
    toast('请使用 PairRoom Service 启动输出中的完整 Management Shell 地址。', 'error');
    return;
  }
  refresh();
  setInterval(refresh, 1500);
})();
