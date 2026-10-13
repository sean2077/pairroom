(() => {
  "use strict";
  const $ = (id) => document.getElementById(id), apiRoot = "/enterprise/api/v1";
  const validID = (value) => typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(value);
  const stale = new Error("stale scope"), inflight = new Set(), lanes = new Map(), cursors = new Map();
  let account = null, csrf = "", epoch = 0, roomVersion = 0, room = null, project = null;
  let pending = null, retryAllowed = false, storageAvailable = true, pendingLogout = null;
  let announced = "", refreshQueued = false;
  const channel = typeof BroadcastChannel === "function" ? new BroadcastChannel("pairroom.enterprise.account.v1") : null;
  const listIDs = ["room-list", "project-list", "device-list", "agent-list", "session-list", "room-members", "project-members", "slots", "messages", "binding-requests", "governance-actions", "pending-list"];
  const inputIDs = ["room-id", "create-room-name", "collaboration-instructions", "create-project-name", "project-name", "project-member-id", "room-name", "room-project-id", "member-id", "ownership-target", "message-text", "governance-room", "governance-owner", "governance-target", "governance-revision"];
  const textIDs = ["room-identity", "room-state", "room-sequence", "project-title", "project-state", "pending-description", "journal-status"];
  const scope = (value = account) => value ? [value.deployment_id, value.user_id, value.session_id].join("/") : "";
  const status = (text) => { $("status").textContent = text; };
  function element(tag, text, className) {
    const value = document.createElement(tag);
    if (text !== undefined) value.textContent = text;
    if (className) value.className = className;
    return value;
  }
  function button(text, fn, disabled = false) {
    const value = element("button", text); value.type = "button"; value.disabled = disabled;
    value.addEventListener("click", fn); return value;
  }
  function clearRoom() {
    room = null; roomVersion += 1; $("room").hidden = true; $("room-title").textContent = "Room";
    lanes.set("room-members", (lanes.get("room-members") || 0) + 1);
    for (const id of ["messages", "slots", "room-members", "binding-requests"]) $(id).replaceChildren();
    for (const id of ["room-name", "room-project-id", "member-id", "ownership-target", "message-text"]) $(id).value = "";
    for (const id of ["room-identity", "room-state", "room-sequence"]) $(id).textContent = "";
  }
  function invalidate() {
    epoch += 1; for (const controller of inflight) controller.abort(); inflight.clear();
    account = null; csrf = ""; pending = null; retryAllowed = false; project = null;
    lanes.clear(); cursors.clear(); clearRoom();
    for (const id of listIDs) $(id).replaceChildren();
    for (const id of inputIDs) $(id).value = "";
    for (const id of textIDs) $(id).textContent = "";
    $("import-operation").value = ""; $("account-state").textContent = "";
    $("workspace").hidden = true; $("pending").hidden = true; $("project-detail").hidden = true;
    $("sign-out").hidden = true; $("room-id").disabled = true; $("open-room").disabled = true;
    $("tab-governance").hidden = true; status(""); return epoch;
  }
  function announce(value, deployment) {
    if (announced === scope(value)) return;
    announced = scope(value);
    if (channel) channel.postMessage({ deployment: value ? value.deployment_id : deployment, user: value ? value.user_id : "", session: value ? value.session_id : "" });
  }
  function signedOut(message, notify = false) {
    const deployment = account && account.deployment_id;
    invalidate(); pendingLogout = null; $("account-state").textContent = message;
    $("sign-in").hidden = false; $("refresh").disabled = false; $("sign-out").disabled = false;
    if (notify && deployment) announce(null, deployment);
  }
  function queueRefresh() {
    if (refreshQueued) return;
    refreshQueued = true;
    queueMicrotask(() => { refreshQueued = false; refreshAccount(false); });
  }
  if (channel) channel.onmessage = ({ data }) => {
    if (!data || typeof data.deployment !== "string" || typeof data.user !== "string" || typeof data.session !== "string") return;
    if (account && data.deployment !== account.deployment_id) return;
    const incoming = data.user && data.session ? [data.deployment, data.user, data.session].join("/") : "";
    if (incoming === scope()) return;
    announced = incoming;
    if (!incoming) { signedOut("Account changed in another tab. Refresh to check your session."); return; }
    invalidate(); status("Account changed in another tab. Checking your session…"); queueRefresh();
  };
  function responseScope(response, expected) {
    return response.headers && response.headers.get("X-PairRoom-User") === expected.user_id && response.headers.get("X-PairRoom-Session") === expected.session_id;
  }
  async function api(path, body) {
    if (!account || !csrf) throw stale;
    const generation = epoch, expected = account, proof = csrf, controller = new AbortController();
    inflight.add(controller);
    const current = () => epoch === generation && scope() === scope(expected) && !controller.signal.aborted;
    try {
      const options = { credentials: "same-origin", cache: "no-store", redirect: "error", signal: controller.signal };
      if (body !== undefined) Object.assign(options, { method: "POST", headers: { "Content-Type": "application/json", "X-PairRoom-CSRF": proof }, body: JSON.stringify(body) });
      const response = await fetch(apiRoot + path, options);
      if (!current()) throw stale;
      if (response.status === 401) { signedOut("Your session has ended. Sign in again.", true); throw stale; }
      if (!responseScope(response, expected)) { invalidate(); queueRefresh(); throw stale; }
      const data = await response.json();
      if (!current()) throw stale;
      return { ok: response.ok, status: response.status, data };
    } finally { inflight.delete(controller); }
  }
  async function refreshAccount(notify = true) {
    const generation = invalidate(), controller = new AbortController(); inflight.add(controller);
    const current = () => epoch === generation && !controller.signal.aborted;
    $("account-state").textContent = "Checking your session…"; $("sign-in").hidden = true; $("refresh").disabled = true;
    try {
      const response = await fetch(apiRoot + "/session", { credentials: "same-origin", cache: "no-store", redirect: "error", signal: controller.signal });
      if (!current()) return;
      if (response.status === 401) { signedOut("Sign in to view your Rooms."); return; }
      if (!response.ok) throw new Error("unavailable");
      const value = await response.json();
      if (!current()) return;
      if (!validID(value.user_id) || !validID(value.session_id) || !validID(value.deployment_id) || typeof value.csrf !== "string" || !value.csrf || !responseScope(response, value)) throw new Error("invalid session");
      account = { user_id: value.user_id, session_id: value.session_id, deployment_id: value.deployment_id, administrator: value.administrator === true }; csrf = value.csrf;
      if (pendingLogout && pendingLogout.scope !== scope()) pendingLogout = null;
      $("account-state").textContent = value.user_id + (value.administrator ? " · Organization administrator" : "");
      $("sign-in").hidden = true; $("sign-out").hidden = false; $("sign-out").disabled = false;
      $("workspace").hidden = false; $("room-id").disabled = false; $("open-room").disabled = false;
      $("tab-governance").hidden = !account.administrator;
      if (notify) announce(account); else announced = scope();
      restoreJournal(); showPanel("rooms"); loadRooms();
    } catch { if (current()) signedOut("Your session could not be checked. Refresh to try again."); }
    finally { inflight.delete(controller); if (epoch === generation) $("refresh").disabled = false; }
  }
  function showPanel(name) { for (const panel of ["rooms", "projects", "devices", "governance"]) $(panel + "-panel").hidden = panel !== name; }
  async function page(resource, params, target, render, more = false) {
    if (!account) return;
    const lane = (lanes.get(target) || 0) + 1, generation = epoch; lanes.set(target, lane);
    if (!more) { $(target).replaceChildren(); cursors.delete(target); }
    const query = new URLSearchParams({ limit: "25", ...params });
    if (more && cursors.get(target)) query.set("after", cursors.get(target));
    try {
      const result = await api("/catalog/" + resource + "?" + query);
      if (generation !== epoch || lanes.get(target) !== lane) return;
      if (!result.ok) throw new Error("unavailable");
      const rows = result.data[resource.endsWith("-members") ? "members" : resource] || [];
      for (const row of rows) $(target).append(render(row));
      cursors.set(target, result.data.next || "");
      const moreID = { "room-list": "rooms-more", "project-list": "projects-more", "device-list": "devices-more", "agent-list": "agents-more", "session-list": "sessions-more", "room-members": "room-members-more", "project-members": "project-members-more" }[target];
      if (moreID) $(moreID).hidden = !result.data.next;
      if (!more && !rows.length) $(target).append(element("li", "No items in this view.", "empty"));
    } catch (error) { if (error !== stale && generation === epoch && lanes.get(target) === lane) status("This view is unavailable. Refresh before taking an action."); }
  }
  function loadRooms(more = false) {
    return page("rooms", { state: $("rooms-state").value || "active" }, "room-list", (row) => {
      const item = element("li"); item.append(button(row.name || row.room_id, () => openRoom(row.room_id)), element("p", row.room_id + " · " + row.role + (row.archived ? " · Archived" : "") + (row.ownership_pending_recovery ? " · Ownership pending recovery" : "") + (row.project_name ? " · " + row.project_name : ""), "muted")); return item;
    }, more);
  }
  function loadProjects(more = false) {
    return page("projects", { state: "all" }, "project-list", (row) => {
      const item = element("li"); item.append(button(row.name, () => selectProject(row)), element("p", row.project_id + " · " + row.role + (row.archived ? " · Archived" : ""), "muted")); return item;
    }, more);
  }
  function owner() { return room && account && room.owner_user_id === account.user_id && room.role === "owner" && !room.ownership_pending_recovery; }
  async function openRoom(id) {
    if (!account || !validID(id)) { status("Enter a valid Room ID."); return; }
    clearRoom(); const version = roomVersion, generation = epoch;
    const current = () => epoch === generation && roomVersion === version;
    $("open-room").disabled = true; status("Opening Room…");
    try {
      const metadata = await api("/catalog/room?room_id=" + encodeURIComponent(id));
      if (!current()) return;
      if (!metadata.ok || !metadata.data.rooms || metadata.data.rooms.length !== 1 || metadata.data.rooms[0].room_id !== id) throw new Error("unavailable");
      const history = await api("/rooms/" + encodeURIComponent(id));
      if (!current()) return;
      if (!history.ok || history.data.room_id !== id || !Number.isSafeInteger(history.data.revision) || history.data.revision !== metadata.data.rooms[0].revision) throw new Error("unavailable");
      room = metadata.data.rooms[0]; room.revision = history.data.revision;
      $("room-title").textContent = room.name || id; $("room-identity").textContent = id; $("room-name").value = room.name; $("room-project-id").value = room.project_id || "";
      $("room-state").textContent = room.ownership_pending_recovery ? "Ownership pending recovery." : room.archived ? "Archived. Agent reservations remain; native work is not stopped." : "Active · " + room.role;
      const messages = history.data.messages || [];
      $("room-sequence").textContent = messages.length + " messages · revision " + history.data.revision;
      for (const slot of ["slot1", "slot2"]) {
        const binding = (history.data.bindings || {})[slot], item = element("li");
        const text = !binding ? "Vacant" : binding.active ? binding.runtime + " · generation " + binding.generation + " · park " + (binding.park_enabled ? "enabled" : "disabled") : "Retired · generation " + binding.generation;
        item.append(element("strong", slot === "slot1" ? "Agent 1" : "Agent 2"), element("p", text), element("p", "Collector activity is not currently observed in this view.", "muted")); $("slots").append(item);
      }
      for (const message of messages) {
        const item = element("li"); item.append(element("p", message.from + " → " + message.to + " · " + message.state, "message-label"), element("p", message.text, "message-text"));
        if (owner() && !room.archived && validID(message.id)) {
          if (message.state === "queued") item.append(button("Cancel queued message", () => queueControl("cancel", message.id)));
          if (message.state === "unknown") {
            const pendingRetry = messages.some((value) => value.retry_of === message.id && ["queued", "delivering", "unknown"].includes(value.state));
            const generationChanged = message.to !== "user" && (history.data.target_generations || {})[message.to] !== message.target_generation;
            item.append(element("p", generationChanged ? "The target Agent changed. Inspect the new peer before deliberately sending new work." : "Check the native session's effects before retrying. A retry creates new queued work.", "muted"));
            item.append(button(pendingRetry ? "Retry already pending" : "Retry after checking effects", () => queueControl("retry", message.id), pendingRetry || generationChanged));
          }
        }
        $("messages").append(item);
      }
      for (const id of ["rename-room", "archive-room", "room-project-save", "member-add", "ownership-transfer", "binding-requests-refresh"]) $(id).disabled = !owner() || room.archived;
      $("archive-room").hidden = room.archived; $("restore-room").hidden = !room.archived; $("restore-room").disabled = !owner();
      $("send-message").disabled = room.archived || !["owner", "member"].includes(room.role);
      $("room").hidden = false; status(messages.length ? "" : "This Room has no messages yet."); loadRoomMembers();
    } catch (error) { if (error !== stale && current()) { clearRoom(); status("This Room is unavailable for your account."); } }
    finally { if (current()) $("open-room").disabled = false; }
  }
  function memberRow(value, forProject) {
    const item = element("li"); item.append(element("span", value.user_id + " · " + value.role + (value.disabled ? " · Disabled" : "")));
    const canEdit = forProject ? project && project.owner_user_id === account.user_id && !project.archived : owner() && !room.archived;
    if (canEdit && value.role !== "owner" && !value.disabled) {
      item.append(button("Remove", () => forProject ? projectCommand("project.member.remove", { target_user_id: value.user_id }) : membership("remove", value.user_id)));
      if (!forProject) item.append(button(value.role === "viewer" ? "Make Member" : "Make Viewer", () => membership("change_role", value.user_id, value.role === "viewer" ? "member" : "viewer")));
    }
    return item;
  }
  function loadRoomMembers(more = false) { if (room) return page("room-members", { room_id: room.room_id }, "room-members", (value) => memberRow(value, false), more); }
  function loadProjectMembers(more = false) { if (project) return page("project-members", { project_id: project.project_id }, "project-members", (value) => memberRow(value, true), more); }
  function selectProject(value) {
    project = value; $("project-detail").hidden = false; $("project-title").textContent = value.name; $("project-name").value = value.name;
    $("project-state").textContent = value.project_id + (value.archived ? " · Archived" : " · Active");
    const allowed = value.owner_user_id === account.user_id && value.role === "owner";
    $("project-rename").disabled = !allowed || value.archived; $("project-member-add").disabled = !allowed || value.archived;
    $("project-archive").hidden = value.archived; $("project-archive").disabled = !allowed; $("project-restore").hidden = !value.archived; $("project-restore").disabled = !allowed;
    loadProjectMembers();
  }
  function connections(resource, more = false) {
    return page(resource, {}, { devices: "device-list", agents: "agent-list", sessions: "session-list" }[resource], (value) => {
      const item = element("li");
      if (resource === "devices") { item.append(element("strong", value.label), element("p", value.device_id + " · " + value.status), button("Revoke Device", () => mutate("/devices/" + value.device_id + "/revoke", "/devices/" + value.device_id + "/resolve-revoke", {}, "Revoke Device"), value.status === "retired")); }
      if (resource === "agents") { item.append(element("strong", value.runtime || "Runtime not recorded"), element("p", value.agent_id + " · Device " + value.device_id + " · " + value.status), button("Retire Agent", () => catalogCommand({ kind: "agent.retire", agent_id: value.agent_id }, 0, "Retire Agent"), value.status === "retired")); }
      if (resource === "sessions") { item.append(element("strong", value.current ? "This browser session" : "Browser session"), element("p", value.session_id + " · " + value.status + " · expires " + new Date(value.expires_at).toLocaleString())); if (!value.current) item.append(button("Revoke session", () => catalogCommand({ kind: "session.revoke", session_id: value.session_id }, 0, "Revoke browser session"), value.status !== "active")); }
      return item;
    }, more);
  }
  const journalPrefix = "pairroom.enterprise.pending.v1:";
  const exactKeys = (value, allowed) => value && typeof value === "object" && !Array.isArray(value) && Object.keys(value).every((key) => allowed.includes(key));
  const revision = (value) => Number.isSafeInteger(value) && value >= 0;
  function journalKey(op) { return journalPrefix + JSON.stringify([op.scope.origin, op.scope.deployment, op.scope.user, op.scope.room, op.body.client_id]); }
  function validOperation(op) {
    if (!account || !op || op.version !== 1 || !op.scope || op.scope.origin !== window.location.origin || op.scope.deployment !== account.deployment_id || op.scope.user !== account.user_id || !op.body || !validID(op.body.client_id) || typeof op.label !== "string" || op.label.length > 100) return false;
    if (!exactKeys(op, ["version", "scope", "path", "resolvePath", "body", "label", "created_at"]) || !exactKeys(op.scope, ["origin", "deployment", "user", "room"])) return false;
    const id = op.scope.room;
    if (id !== "" && !validID(id)) return false;
    const body = op.body;
    if (op.path === "/catalog/commands" && op.resolvePath === "/catalog/resolve") return exactKeys(body, ["client_id", "expected_revision", "intent"]) && revision(body.expected_revision) && exactKeys(body.intent, ["kind", "room_id", "project_id", "target_user_id", "session_id", "agent_id", "name"]) && (body.intent.room_id || "") === id;
    if (op.path === "/rooms" && id === body.room_id && op.resolvePath === "/rooms/" + id + "/resolve-create") return exactKeys(body, ["client_id", "room_id", "intent"]) && exactKeys(body.intent, ["name", "collaboration"]) && exactKeys(body.intent.collaboration, ["version", "mode", "instructions"]);
    if (id && op.path === "/rooms/" + id + "/messages" && op.resolvePath === "/rooms/" + id + "/resolve-message") return exactKeys(body, ["client_id", "to", "text"]) && ["slot1", "slot2"].includes(body.to) && typeof body.text === "string";
    if (id && op.path === "/rooms/" + id + "/memberships" && op.resolvePath === "/rooms/" + id + "/resolve-membership") return exactKeys(body, ["client_id", "expected_revision", "intent"]) && revision(body.expected_revision) && exactKeys(body.intent, ["action", "target_user_id", "role"]);
    if (id && op.path === "/rooms/" + id + "/ownership" && op.resolvePath === "/rooms/" + id + "/resolve-ownership") return exactKeys(body, ["client_id", "intent", "action_id"]) && exactKeys(body.intent, ["room_id", "target_user_id", "expected_owner_user_id", "expected_revision"]) && body.intent.room_id === id && revision(body.intent.expected_revision);
    if (id && op.path === "/rooms/" + id + "/queue" && op.resolvePath === "/rooms/" + id + "/resolve-queue") return exactKeys(body, ["client_id", "expected_revision", "intent"]) && revision(body.expected_revision) && exactKeys(body.intent, ["kind", "message_id"]) && ["retry", "cancel"].includes(body.intent.kind) && validID(body.intent.message_id);
    return typeof op.path === "string" && /^\/devices\/[A-Za-z0-9][A-Za-z0-9._-]{0,127}\/revoke$/.test(op.path) && op.resolvePath === op.path.replace(/\/revoke$/, "/resolve-revoke") && exactKeys(body, ["client_id"]);
  }
  function persist(op) {
    try {
      const encoded = JSON.stringify(op), key = journalKey(op), storage = window.localStorage;
      let count = 0, bytes = encoded.length;
      for (let i = 0; i < storage.length; i += 1) { const other = storage.key(i); if (other.startsWith(journalPrefix) && other !== key) { count += 1; bytes += (storage.getItem(other) || "").length; } }
      if (encoded.length > 600000 || count >= 20 || bytes > 1200000) throw new Error("journal full");
      storage.setItem(key, encoded); storageAvailable = true;
    } catch { storageAvailable = false; }
  }
  function showPending() {
    $("pending").hidden = !pending; if (!pending) return;
    $("pending-description").textContent = pending.label + (pending.scope.room ? " · " + pending.scope.room : "");
    $("retry-operation").hidden = !retryAllowed;
    $("discard-operation").hidden = !retryAllowed;
    $("journal-status").textContent = storageAvailable ? "This account's recovery record is saved in this browser." : "Browser storage is unavailable. Export this record before leaving; reload recovery is unavailable.";
  }
  function restoreJournal() {
    pending = null; retryAllowed = false; $("pending-list").replaceChildren();
    try {
      const storage = window.localStorage;
      let count = 0, bytes = 0;
      for (let i = 0; i < storage.length && i < 1000; i += 1) {
        const key = storage.key(i); if (!key.startsWith(journalPrefix)) continue;
        const encoded = storage.getItem(key); if (!encoded || encoded.length > 600000) continue;
        let op; try { op = JSON.parse(encoded); } catch { continue; }
        if (!validOperation(op) || key !== journalKey(op)) continue;
        bytes += encoded.length; count += 1;
        if (count > 20 || bytes > 1200000) break;
        if (!pending) pending = op;
        $("pending-list").append(button(op.label + (op.scope.room ? " · " + op.scope.room : ""), () => { pending = op; retryAllowed = false; showPending(); }));
      }
    } catch { storageAvailable = false; }
    showPending();
  }
  async function runOperation(op, resolveOnly) {
    if (!validOperation(op)) return;
    pending = op; retryAllowed = false; persist(op); showPending();
    const generation = epoch; $("resolve-operation").disabled = true; $("retry-operation").disabled = true;
    try {
      const body = resolveOnly && op.path === "/rooms" ? { client_id: op.body.client_id, intent: op.body.intent } : op.body;
      const result = await api(resolveOnly ? op.resolvePath : op.path, body);
      if (epoch !== generation || pending !== op) return;
      if (resolveOnly && result.data.outcome === "not_committed") { retryAllowed = true; status("The action was not committed. You may explicitly retry the same action."); return; }
      if (!result.ok || result.data.outcome === "unknown" || result.data.outcome && !["committed", "replayed"].includes(result.data.outcome)) { status("This action is not confirmed. Check its original result before taking another action."); return; }
      try { window.localStorage.removeItem(journalKey(op)); } catch { storageAvailable = false; }
      pending = null; restoreJournal(); status("Action confirmed.");
      if (op.scope.room && (!room || room.room_id === op.scope.room)) openRoom(op.scope.room);
      loadRooms();
      if (!$("projects-panel").hidden) { project = null; $("project-detail").hidden = true; loadProjects(); }
      if (!$("devices-panel").hidden) for (const resource of ["devices", "agents", "sessions"]) connections(resource);
      if (!$("governance-panel").hidden) loadGovernance();
    } catch (error) { if (error !== stale && epoch === generation) status("The result is uncertain. Its original recovery identity is retained."); }
    finally { if (epoch === generation) { $("resolve-operation").disabled = false; $("retry-operation").disabled = false; showPending(); } }
  }
  function mutate(path, resolvePath, body, label, roomID = "") {
    if (!account || pending) { status("Check the pending action before starting another mutation."); return; }
    return runOperation({ version: 1, scope: { origin: window.location.origin, deployment: account.deployment_id, user: account.user_id, room: roomID }, path, resolvePath, body: { client_id: crypto.randomUUID(), ...body }, label, created_at: new Date().toISOString() }, false);
  }
  function catalogCommand(intent, revision, label) { return mutate("/catalog/commands", "/catalog/resolve", { expected_revision: revision, intent }, label, intent.room_id || ""); }
  function roomCommand(kind, extra = {}) { if (owner()) return catalogCommand({ kind, room_id: room.room_id, ...extra }, room.revision, kind); }
  function queueControl(kind, messageID) {
    if (!owner() || room.archived || !validID(messageID)) return;
    return mutate("/rooms/" + room.room_id + "/queue", "/rooms/" + room.room_id + "/resolve-queue", { expected_revision: room.revision, intent: { kind, message_id: messageID } }, kind === "retry" ? "Retry unknown delivery" : "Cancel queued message", room.room_id);
  }
  function projectCommand(kind, extra = {}) { if (project) return catalogCommand({ kind, project_id: project.project_id, ...extra }, project.revision, kind); }
  function membership(action, target, role) {
    if (!owner()) return;
    const id = room.room_id, intent = { action, target_user_id: target }; if (role) intent.role = role;
    return mutate("/rooms/" + id + "/memberships", "/rooms/" + id + "/resolve-membership", { expected_revision: room.revision, intent }, "Change Room membership", id);
  }
  function transfer(intent, actionID) { return mutate("/rooms/" + intent.room_id + "/ownership", "/rooms/" + intent.room_id + "/resolve-ownership", { intent, ...(actionID ? { action_id: actionID } : {}) }, "Transfer Room ownership", intent.room_id); }
  async function loadBindings() {
    if (!owner()) return;
    const id = room.room_id, version = roomVersion; $("binding-requests").replaceChildren();
    try {
      const result = await api("/rooms/" + id + "/binding-requests"); if (version !== roomVersion || !result.ok) return;
      for (const value of result.data.requests || []) {
        const link = element("a", value.runtime + " · " + value.slot + " · " + value.status + (value.replace ? " · Replacement" : ""));
        link.href = "/enterprise/binding-requests/" + encodeURIComponent(id) + "/" + encodeURIComponent(value.request_id); link.target = "_blank"; link.rel = "noopener noreferrer";
        const item = element("li"); item.append(link); $("binding-requests").append(item);
      }
    } catch (error) { if (error !== stale && version === roomVersion) status("Agent requests are unavailable."); }
  }
  async function loadGovernance() {
    if (!account || !account.administrator) return;
    const generation = epoch; $("governance-actions").replaceChildren();
    try {
      const result = await api("/governance/actions"); if (generation !== epoch || !result.ok) return;
      for (const action of result.data.actions || []) { const item = element("li"); item.append(element("p", action.intent.room_id + " → " + action.intent.target_user_id + " · " + (action.ready ? "Identity verified" : "Verification required")), button("Transfer this Room", () => transfer(action.intent, action.id), !action.ready || Date.parse(action.expires_at) <= Date.now())); $("governance-actions").append(item); }
    } catch (error) { if (error !== stale && generation === epoch) status("Prepared ownership actions are unavailable."); }
  }
  function form(id, fn) { $(id).addEventListener("submit", (event) => { event.preventDefault(); if (account && csrf) return fn(); }); }
  $("refresh").addEventListener("click", () => refreshAccount());
  for (const panel of ["rooms", "projects", "devices", "governance"]) $("tab-" + panel).addEventListener("click", () => { showPanel(panel); if (panel === "rooms") loadRooms(); if (panel === "projects") loadProjects(); if (panel === "devices") for (const resource of ["devices", "agents", "sessions"]) connections(resource); if (panel === "governance") loadGovernance(); });
  const actions = { "rooms-refresh": () => loadRooms(), "rooms-more": () => loadRooms(true), "projects-refresh": () => loadProjects(), "projects-more": () => loadProjects(true), "devices-refresh": () => { for (const value of ["devices", "agents", "sessions"]) connections(value); }, "devices-more": () => connections("devices", true), "agents-more": () => connections("agents", true), "sessions-more": () => connections("sessions", true), "room-members-more": () => loadRoomMembers(true), "project-members-more": () => loadProjectMembers(true), "room-refresh": () => room && openRoom(room.room_id), "binding-requests-refresh": loadBindings, "governance-refresh": loadGovernance, "project-archive": () => projectCommand("project.archive"), "project-restore": () => projectCommand("project.restore"), "archive-room": () => roomCommand("room.archive"), "restore-room": () => roomCommand("room.restore") };
  for (const [id, fn] of Object.entries(actions)) $(id).addEventListener("click", fn);
  $("rooms-state").addEventListener("change", () => loadRooms());
  $("collaboration-mode").addEventListener("change", () => { $("collaboration-instructions").disabled = $("collaboration-mode").value !== "custom"; });
  form("room-form", () => openRoom($("room-id").value.trim()));
  form("create-room-form", async () => {
    if (pending) return;
    const generation = epoch, custom = $("collaboration-mode").value === "custom";
    try {
      const result = await api("/room-template", { name: $("create-room-name").value.trim(), collaboration: custom ? { mode: "custom", instructions: $("collaboration-instructions").value } : { mode: "default" } });
      if (generation !== epoch || !result.ok || !result.data.intent) return;
      const id = "room-" + crypto.randomUUID();
      return mutate("/rooms", "/rooms/" + id + "/resolve-create", { room_id: id, intent: result.data.intent }, "Create Room", id);
    } catch (error) { if (error !== stale && generation === epoch) status("The Room template could not be prepared."); }
  });
  form("create-project-form", () => catalogCommand({ kind: "project.create", project_id: "project-" + crypto.randomUUID(), name: $("create-project-name").value.trim() }, 0, "Create Project"));
  form("project-rename-form", () => projectCommand("project.rename", { name: $("project-name").value.trim() }));
  form("project-member-form", () => projectCommand("project.member.add", { target_user_id: $("project-member-id").value.trim() }));
  form("rename-room-form", () => roomCommand("room.rename", { name: $("room-name").value.trim() }));
  form("room-project-form", () => roomCommand("room.project", { project_id: $("room-project-id").value.trim() }));
  form("member-form", () => membership("admit", $("member-id").value.trim(), $("member-role").value));
  form("ownership-form", () => owner() && transfer({ room_id: room.room_id, target_user_id: $("ownership-target").value.trim(), expected_owner_user_id: room.owner_user_id, expected_revision: room.revision }));
  form("send-form", () => { if (room && !$("send-message").disabled) return mutate("/rooms/" + room.room_id + "/messages", "/rooms/" + room.room_id + "/resolve-message", { to: $("send-target").value, text: $("message-text").value }, "Send message", room.room_id); });
  form("governance-form", async () => {
    if (!account.administrator || pending) return;
    const generation = epoch, intent = { room_id: $("governance-room").value.trim(), expected_owner_user_id: $("governance-owner").value.trim(), target_user_id: $("governance-target").value.trim(), expected_revision: Number($("governance-revision").value) };
    if (!Number.isSafeInteger(intent.expected_revision) || intent.expected_revision < 1) return;
    try {
      const result = await api("/governance/actions", { client_id: crypto.randomUUID(), intent });
      if (generation !== epoch || !result.ok) return;
      if (result.data.authorization_url) { const url = new URL(result.data.authorization_url); if (url.protocol !== "https:" || url.username || url.password) throw new Error("invalid provider URL"); window.location.assign(url.href); } else loadGovernance();
    } catch (error) { if (error !== stale && generation === epoch) status("Ownership verification could not be started."); }
  });
  $("resolve-operation").addEventListener("click", () => pending && runOperation(pending, true));
  $("retry-operation").addEventListener("click", () => pending && retryAllowed && runOperation(pending, false));
  $("discard-operation").addEventListener("click", () => {
    if (!pending || !retryAllowed) return;
    try { window.localStorage.removeItem(journalKey(pending)); } catch { storageAvailable = false; }
    pending = null; retryAllowed = false; restoreJournal(); status("The uncommitted action was discarded. Refresh before preparing a new action.");
  });
  $("import-operation").addEventListener("change", async () => {
    const generation = epoch, file = $("import-operation").files && $("import-operation").files[0];
    if (!account || pending || !file || file.size > 600000) return;
    try {
      const encoded = await file.text();
      if (generation !== epoch) return;
      const op = JSON.parse(encoded);
      if (!validOperation(op)) throw new Error("wrong scope");
      pending = op; retryAllowed = false; persist(op); showPending(); status("Recovery record loaded. Check the original result before retrying.");
    } catch { if (generation === epoch) status("This recovery record is invalid or belongs to another account or deployment."); }
    finally { if (generation === epoch) $("import-operation").value = ""; }
  });
  $("export-operation").addEventListener("click", () => {
    if (!pending) return;
    const url = URL.createObjectURL(new Blob([JSON.stringify(pending, null, 2)], { type: "application/json" })), link = element("a");
    link.href = url; link.download = "pairroom-pending-" + pending.body.client_id + ".json"; link.click(); URL.revokeObjectURL(url);
  });
  $("sign-out").addEventListener("click", async () => {
    if (!account || !csrf) return;
    const selected = account, proof = csrf;
    if (!pendingLogout || pendingLogout.scope !== scope()) pendingLogout = { id: crypto.randomUUID(), scope: scope() };
    const operation = pendingLogout, generation = invalidate(), controller = new AbortController(); inflight.add(controller);
    announce(null, selected.deployment_id); $("account-state").textContent = "Signing out…"; $("sign-out").disabled = true;
    try {
      const response = await fetch(apiRoot + "/session/logout", { method: "POST", credentials: "same-origin", cache: "no-store", redirect: "error", signal: controller.signal, headers: { "Content-Type": "application/json", "X-PairRoom-CSRF": proof }, body: JSON.stringify({ client_id: operation.id }) });
      if (generation !== epoch || controller.signal.aborted) return;
      if (response.status === 401) { signedOut("Your session has ended."); return; }
      if (!responseScope(response, selected)) { queueRefresh(); return; }
      if (!response.ok) throw new Error("unknown");
      signedOut("You are signed out.");
    } catch { if (generation === epoch) status("Sign-out could not be confirmed. Refresh your account before trying again."); }
    finally { inflight.delete(controller); if (generation === epoch) $("sign-out").disabled = false; }
  });
  refreshAccount();
})();
