(() => {
  "use strict";
  const $ = (id) => document.getElementById(id), apiRoot = "/enterprise/api/v1";
  const validID = (value) => typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(value);
  const validName = (value) => typeof value === "string" && value.length > 0 && value.trim() === value && new TextEncoder().encode(value).length <= 160 && !/\p{Cc}/u.test(value);
  const stale = new Error("stale scope"), inflight = new Set(), lanes = new Map(), cursors = new Map();
  const journalRecords = new Map(), absentOperations = new Set(), busyOperations = new Set();
  const attachmentDrafts = new Map(), selectedFiles = new Map(), includedAttachments = new Set(), busyAttachments = new Set();
  let account = null, csrf = "", epoch = 0, roomVersion = 0, room = null, project = null;
  let pending = null, retryAllowed = false, storageAvailable = true, pendingLogout = null;
  let announced = "", refreshQueued = false;
  let selectedRoomID = "";
  let displayedMessages = [], historyTotal = 0, historyCursor = "", historySequence = 0, historyBusy = false, historyChanged = false, targetGenerations = {};
  const channel = typeof BroadcastChannel === "function" ? new BroadcastChannel("pairroom.enterprise.account.v1") : null;
  const listIDs = ["room-list", "project-list", "device-list", "agent-list", "session-list", "room-members", "project-members", "slots", "messages", "binding-requests", "governance-actions", "pending-list", "administration-users", "administration-administrators", "administration-admissions", "administration-connections"];
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
    displayedMessages = []; historyTotal = 0; historyCursor = ""; historySequence = 0; historyBusy = false; historyChanged = false; targetGenerations = {};
    $("history-older").hidden = true; $("history-older").disabled = false; $("history-status").textContent = "";
    lanes.set("room-members", (lanes.get("room-members") || 0) + 1);
    for (const id of ["messages", "slots", "room-members", "binding-requests"]) $(id).replaceChildren();
    for (const id of ["room-name", "room-project-id", "member-id", "ownership-target", "message-text"]) $(id).value = "";
    for (const id of ["room-identity", "room-state", "room-sequence"]) $(id).textContent = "";
    $("attachment-file").value = ""; $("attachment-drafts").replaceChildren();
  }
  function invalidate() {
    epoch += 1; for (const controller of inflight) controller.abort(); inflight.clear();
    account = null; csrf = ""; pending = null; retryAllowed = false; project = null;
    journalRecords.clear(); absentOperations.clear(); busyOperations.clear(); selectedRoomID = "";
    attachmentDrafts.clear(); selectedFiles.clear(); includedAttachments.clear(); busyAttachments.clear();
    lanes.clear(); cursors.clear(); clearRoom();
    for (const id of listIDs) $(id).replaceChildren();
    for (const id of inputIDs) $(id).value = "";
    for (const id of textIDs) $(id).textContent = "";
    $("import-operation").value = ""; $("account-state").textContent = "";
    $("workspace").hidden = true; $("pending").hidden = true; $("project-detail").hidden = true;
    $("sign-out").hidden = true; $("room-id").disabled = true; $("open-room").disabled = true;
    for (const id of ["admission-connection", "admission-issuer", "admission-subject"]) $(id).value = "";
    $("tab-governance").hidden = true; $("tab-administration").hidden = true; status(""); return epoch;
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
  async function api(path, body, rawUpload = false) {
    if (!account || !csrf) throw stale;
    const generation = epoch, expected = account, proof = csrf, controller = new AbortController();
    inflight.add(controller);
    const current = () => epoch === generation && scope() === scope(expected) && !controller.signal.aborted;
    try {
      const options = { credentials: "same-origin", cache: "no-store", redirect: "error", signal: controller.signal };
      if (body !== undefined) Object.assign(options, { method: rawUpload ? "PUT" : "POST", headers: { "Content-Type": rawUpload ? "application/octet-stream" : "application/json", "X-PairRoom-CSRF": proof }, body: rawUpload ? body : JSON.stringify(body) });
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
      $("tab-administration").hidden = !account.administrator;
      if (notify) announce(account); else announced = scope();
      restoreJournal(); restoreAttachments(); showPanel("rooms"); loadRooms();
    } catch { if (current()) signedOut("Your session could not be checked. Refresh to try again."); }
    finally { inflight.delete(controller); if (epoch === generation) $("refresh").disabled = false; }
  }
  function showPanel(name) { for (const panel of ["rooms", "projects", "devices", "governance", "administration"]) $(panel + "-panel").hidden = panel !== name; }
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
    clearRoom(); selectedRoomID = id; const version = roomVersion, generation = epoch;
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
      if (!Array.isArray(messages) || messages.length > 300 || !Number.isSafeInteger(history.data.total_messages) || history.data.total_messages < messages.length || !messages.length && history.data.total_messages > 0 || !Number.isSafeInteger(history.data.sequence) || history.data.sequence < 0) throw new Error("invalid history tail");
      displayedMessages = messages; historyTotal = history.data.total_messages; historySequence = history.data.sequence; targetGenerations = history.data.target_generations || {};
      // The tail is a contiguous publication suffix. Its first ordinal is
      // stable across later appends; it is not inferred from a later page total.
      historyCursor = historyTotal > messages.length ? "history:" + (historyTotal - messages.length) : "";
      for (const slot of ["slot1", "slot2"]) {
        const binding = (history.data.bindings || {})[slot], item = element("li");
        const vacant = !binding || binding.bind_id === "" && binding.generation === 0 && binding.active === false;
        const text = vacant ? "Vacant" : binding.active ? binding.runtime + " · generation " + binding.generation + " · park " + (binding.park_enabled ? "enabled" : "disabled") : "Retired · generation " + binding.generation;
        item.append(element("strong", slot === "slot1" ? "Agent 1" : "Agent 2"), element("p", text), element("p", "Collector activity is not currently observed in this view.", "muted")); $("slots").append(item);
      }
      renderRoomMessages();
      for (const id of ["rename-room", "archive-room", "room-project-save", "member-add", "ownership-transfer", "binding-requests-refresh"]) $(id).disabled = !owner() || room.archived;
      $("archive-room").hidden = room.archived; $("restore-room").hidden = !room.archived; $("restore-room").disabled = !owner();
      $("send-message").disabled = room.archived || !["owner", "member"].includes(room.role);
      $("room").hidden = false; status(messages.length ? "" : "This Room has no messages yet."); loadRoomMembers(); renderAttachments();
    } catch (error) { if (error !== stale && current()) { clearRoom(); status("This Room is unavailable for your account."); } }
    finally { if (current()) $("open-room").disabled = false; }
  }
  function renderRoomMessages() {
    if (!room) return;
    $("messages").replaceChildren();
    $("room-sequence").textContent = historyTotal + " messages · " + displayedMessages.length + " loaded · revision " + room.revision;
    $("history-older").hidden = !historyCursor; $("history-older").disabled = historyBusy;
    $("history-status").textContent = historyChanged ? "The Room changed while reading history. Refresh Room to include newer messages and current delivery states." : historyCursor ? "Recent messages are shown. Load older messages to read the retained history." : "All retained messages are loaded.";
    for (const message of displayedMessages) {
        const author = message.from === "user" ? typeof message.author === "string" && message.author.startsWith("enterprise:") && validID(message.author.slice(11)) ? "User " + message.author.slice(11) : message.author || "User" : message.from;
        const item = element("li"); item.append(element("p", author + " → " + message.to + " · " + message.state, "message-label"), element("p", message.text, "message-text"));
        for (const attachment of message.attachments || []) if (validAttachmentID(attachment.id)) { const link = element("a", attachment.name + " · " + attachment.size + " bytes"), detail = element("p"); link.href = apiRoot + "/rooms/" + room.room_id + "/attachments/" + attachment.id + "/content"; link.target = "_blank"; link.rel = "noopener noreferrer"; detail.append(link); item.append(detail); }
        if (owner() && !room.archived && validID(message.id)) {
          if (message.state === "queued") item.append(button("Cancel queued message", () => queueControl("cancel", message.id), historyChanged));
          if (message.state === "unknown") {
            const pendingRetry = displayedMessages.some((value) => value.retry_of === message.id && ["queued", "delivering", "unknown"].includes(value.state));
            const generationChanged = message.to !== "user" && targetGenerations[message.to] !== message.target_generation;
            item.append(element("p", generationChanged ? "The target Agent changed. Inspect the new peer before deliberately sending new work." : "Check the native session's effects before retrying. A retry creates new queued work.", "muted"));
            item.append(button(pendingRetry ? "Retry already pending" : "Retry after checking effects", () => queueControl("retry", message.id), pendingRetry || generationChanged || historyChanged));
          }
        }
        $("messages").append(item);
    }
  }
  async function loadOlderHistory() {
    if (!room || !historyCursor || historyBusy) return;
    const id = room.room_id, cursor = historyCursor, generation = epoch, version = roomVersion;
    const current = () => epoch === generation && roomVersion === version && room && room.room_id === id;
    historyBusy = true; $("history-older").disabled = true; $("history-status").textContent = "Loading older messages…";
    try {
      const result = await api("/rooms/" + id + "/history", { cursor, limit: 100 });
      if (!current()) return;
      if (result.status === 403) { clearRoom(); status("Room access changed. Refresh your account before opening this Room again."); return; }
      const page = result.data;
      if (!result.ok || !Array.isArray(page.messages) || page.messages.length > 100 || !Number.isSafeInteger(page.total) || page.total < historyTotal || !Number.isSafeInteger(page.sequence) || page.sequence < historySequence || typeof page.has_more !== "boolean" || page.messages.some(message => !validID(message.id))) throw new Error("invalid history page");
      if (page.has_more && (typeof page.next_cursor !== "string" || !/^history:(0|[1-9][0-9]*)$/.test(page.next_cursor) || !Number.isSafeInteger(Number(page.next_cursor.slice(8))) || Number(page.next_cursor.slice(8)) >= Number(cursor.slice(8)))) throw new Error("history did not advance");
      const seen = new Set(displayedMessages.map(message => message.id)), older = [];
      for (const message of [...page.messages].reverse()) if (!seen.has(message.id)) { seen.add(message.id); older.push(message); }
      const loaded = older.length + displayedMessages.length, changed = historyChanged || page.sequence !== historySequence;
      if (loaded > page.total || page.has_more && !page.messages.length || !page.has_more && !changed && loaded !== page.total || page.total > historyTotal && page.sequence === historySequence) throw new Error("invalid retained history total");
      displayedMessages = [...older, ...displayedMessages]; historyTotal = page.total; historyChanged = historyChanged || page.sequence !== historySequence;
      historyCursor = page.has_more ? page.next_cursor : ""; renderRoomMessages();
    } catch (error) { if (error !== stale && current()) $("history-status").textContent = "Older history is unavailable. The current page and original cursor are retained; explicitly try loading again."; }
    finally { if (current()) { historyBusy = false; $("history-older").disabled = false; } }
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
  function administrationCommand(intent, label) {
    if (!account || !account.administrator) return;
    return mutate("/administration/commands", "/administration/resolve", { intent }, label);
  }
  async function administrationPage(resource, more = false) {
    if (!account || !account.administrator) return;
    const target = "administration-" + resource, generation = epoch, lane = (lanes.get(target) || 0) + 1;
    lanes.set(target, lane);
    if (!more) { $(target).replaceChildren(); cursors.delete(target); }
    const query = new URLSearchParams({ limit: "25" });
    if (more && cursors.get(target)) query.set("after", cursors.get(target));
    try {
      const result = await api("/administration/" + resource + "?" + query);
      if (generation !== epoch || lanes.get(target) !== lane) return;
      if (!result.ok || result.data.resource !== resource || !Array.isArray(result.data[resource] || [])) throw new Error("unavailable");
      const rows = result.data[resource] || [];
      for (const row of rows) {
        const item = element("li");
        if (resource === "users" || resource === "administrators") {
          item.append(element("strong", row.user_id), element("p", (row.tombstoned ? "Retired" : row.disabled ? "Disabled" : "Enabled") + (row.administrator ? " · Administrator" : " · No administrator role")));
          item.append(button(row.disabled ? "Enable User" : "Disable User", () => administrationCommand({ kind: "user.status", target_user_id: row.user_id, enabled: row.disabled }, row.disabled ? "Enable User" : "Disable User"), row.tombstoned));
          item.append(button(row.administrator ? "Remove administrator role" : "Grant administrator role", () => administrationCommand({ kind: "administrator.set", target_user_id: row.user_id, enabled: !row.administrator }, row.administrator ? "Remove administrator role" : "Grant administrator role"), row.tombstoned || row.disabled && !row.administrator));
        } else if (resource === "admissions") {
          const identity = { connection_id: row.connection_id, issuer: row.issuer, subject: row.subject };
          item.append(element("strong", row.connection_id), element("p", row.issuer), element("pre", row.subject), element("p", (row.enabled ? "Admitted" : "Not admitted") + (row.user_id ? " · User " + row.user_id : " · No User created") + (row.identity_retired ? " · Identity retired" : "")));
          item.append(button(row.enabled ? "Remove admission" : "Admit identity", () => administrationCommand({ kind: "identity.admission", identity, enabled: !row.enabled }, row.enabled ? "Remove identity admission" : "Admit identity"), row.identity_retired));
        } else {
          item.append(element("strong", row.connection_id), element("p", row.issuer), button("Use this connection", () => { $("admission-connection").value = row.connection_id; $("admission-issuer").value = row.issuer; }));
        }
        $(target).append(item);
      }
      cursors.set(target, result.data.next || ""); $(target + "-more").hidden = !result.data.next;
      if (!more && !rows.length) $(target).append(element("li", "No items in this view.", "empty"));
    } catch (error) { if (error !== stale && generation === epoch && lanes.get(target) === lane) status("Administration is unavailable. Refresh your account before taking an action."); }
  }
  function loadAdministration() { for (const resource of ["users", "administrators", "admissions", "connections"]) administrationPage(resource); }
  const attachmentPrefix = "pairroom.enterprise.attachment.v1:";
  const validAttachmentID = value => typeof value === "string" && /^att-[0-9a-f]{24}$/.test(value);
  function attachmentKey(draft) { return attachmentPrefix + JSON.stringify([draft.scope.origin, draft.scope.deployment, draft.scope.user, draft.scope.room, draft.id]); }
  function validAttachmentDraft(draft) {
    if (!account || !exactKeys(draft, ["version", "id", "scope", "spec", "state", "reserve_id", "verify_id", "cancel_id", "object_id"]) || draft.version !== 1 || !validID(draft.id) || !exactKeys(draft.scope, ["origin", "deployment", "user", "room"]) || draft.scope.origin !== window.location.origin || draft.scope.deployment !== account.deployment_id || draft.scope.user !== account.user_id || !validID(draft.scope.room)) return false;
    if (!exactKeys(draft.spec, ["name", "kind", "size", "sha256"]) || typeof draft.spec.name !== "string" || !draft.spec.name || new TextEncoder().encode(draft.spec.name).length > 256 || /[\/\\\p{Cc}]/u.test(draft.spec.name) || !["file", "image"].includes(draft.spec.kind) || !Number.isSafeInteger(draft.spec.size) || draft.spec.size < 1 || draft.spec.size > 5 * 1024 * 1024 || !/^[0-9a-f]{64}$/.test(draft.spec.sha256)) return false;
    return ["prepared", "reserving", "reserved", "uploading", "uploaded", "verifying", "verified", "cancelling", "cancel_ready"].includes(draft.state) && [draft.reserve_id, draft.verify_id, draft.cancel_id].every(validID) && (draft.object_id === "" ? ["prepared", "reserving"].includes(draft.state) : validAttachmentID(draft.object_id));
  }
  function saveAttachment(draft) {
    attachmentDrafts.set(draft.id, draft);
    try { window.localStorage.setItem(attachmentKey(draft), JSON.stringify(draft)); } catch { storageAvailable = false; }
  }
  function removeAttachmentDraft(id) {
    const draft = attachmentDrafts.get(id); if (!draft) return;
    try { window.localStorage.removeItem(attachmentKey(draft)); } catch { storageAvailable = false; }
    attachmentDrafts.delete(id); selectedFiles.delete(id); includedAttachments.delete(id); renderAttachments();
  }
  function restoreAttachments() {
    try {
      const storage = window.localStorage;
      for (let i = 0; i < storage.length && i < 1000 && attachmentDrafts.size < 20; i += 1) {
        const key = storage.key(i); if (!key.startsWith(attachmentPrefix)) continue;
        const text = storage.getItem(key); if (!text || text.length > 10000) continue;
        let draft; try { draft = JSON.parse(text); } catch { continue; }
        if (validAttachmentDraft(draft) && key === attachmentKey(draft)) attachmentDrafts.set(draft.id, draft);
      }
    } catch { storageAvailable = false; }
  }
  async function fileHash(file) { return [...new Uint8Array(await crypto.subtle.digest("SHA-256", await file.arrayBuffer()))].map(value => value.toString(16).padStart(2, "0")).join(""); }
  async function reselectAttachment(draft, file) {
    const generation = epoch, lane = "attachment-file:" + draft.id, version = (lanes.get(lane) || 0) + 1;
    lanes.set(lane, version); selectedFiles.delete(draft.id); renderAttachments();
    try {
      if (!file || file.size !== draft.spec.size || await fileHash(file) !== draft.spec.sha256) throw new Error("different bytes");
      if (generation !== epoch || lanes.get(lane) !== version || !validAttachmentDraft(draft)) return;
      selectedFiles.set(draft.id, file); status("Matching attachment bytes selected. Upload remains explicit."); renderAttachments();
    } catch { if (generation === epoch && lanes.get(lane) === version) status("Select the exact original attachment bytes and size. The recovery identity has been retained."); }
  }
  function renderAttachments() {
    $("attachment-drafts").replaceChildren(); if (!room) return;
    $("prepare-attachment").disabled = room.archived || !["owner", "member"].includes(room.role);
    for (const draft of attachmentDrafts.values()) if (draft.scope.room === room.room_id) {
      const item = element("li"), busy = busyAttachments.has(draft.id), path = "/rooms/" + draft.scope.room + "/attachments";
      item.append(element("strong", draft.spec.name), element("p", draft.spec.kind + " · " + draft.spec.size + " bytes · " + draft.state), element("p", "SHA-256 " + draft.spec.sha256, "muted"));
      item.append(button("Export attachment recovery metadata", () => exportRecord(draft, "pairroom-attachment-" + draft.id + ".json")));
      if (["prepared", "reserved", "uploading"].includes(draft.state)) {
        const input = element("input"); input.type = "file"; input.disabled = busy; input.setAttribute("aria-label", "Reselect exact bytes for " + draft.spec.name);
        input.addEventListener("change", () => reselectAttachment(draft, input.files && input.files[0])); item.append(input);
        item.append(element("p", selectedFiles.has(draft.id) ? "Original bytes are selected in this page." : "Reload retains metadata only. Reselect the original bytes before explicitly uploading.", "muted"));
      }
      if (draft.state === "prepared") item.append(button("Reserve upload", () => attachmentOperation(draft, "reserve"), busy));
      if (draft.state === "reserving") item.append(button("Check original reservation", () => attachmentOperation(draft, "resolve-reservation"), busy));
      if (["reserved", "uploading"].includes(draft.state)) item.append(button(draft.state === "uploading" ? "Retry upload with original bytes" : "Upload selected bytes", () => attachmentOperation(draft, "content"), busy || !selectedFiles.has(draft.id)));
      if (draft.state === "uploaded") item.append(button("Verify uploaded attachment", () => attachmentOperation(draft, "verify"), busy));
      if (draft.state === "verifying") item.append(button("Check original verification", () => attachmentOperation(draft, "resolve-verification"), busy));
      if (draft.state === "cancelling") item.append(button("Check original cancellation", () => attachmentOperation(draft, "resolve-cancellation"), busy));
      if (draft.state === "verified") {
        item.append(button(includedAttachments.has(draft.id) ? "Remove from next message" : "Include in next message", () => { if (includedAttachments.has(draft.id)) includedAttachments.delete(draft.id); else includedAttachments.add(draft.id); renderAttachments(); }, busy));
        const link = element("a", "Download verified draft"); link.href = apiRoot + path + "/" + draft.object_id + "/content"; link.target = "_blank"; link.rel = "noopener noreferrer"; item.append(link);
      }
      if (draft.state === "prepared") item.append(button("Discard unreserved draft", () => removeAttachmentDraft(draft.id), busy));
      const pendingReference = [...journalRecords.values()].some(op => op.scope.room === draft.scope.room && (op.body.attachment_ids || []).includes(draft.object_id));
      if (["reserved", "uploading", "uploaded", "verified", "cancel_ready"].includes(draft.state)) item.append(button("Abandon attachment draft", () => attachmentOperation(draft, "cancel"), busy || pendingReference));
      if (!storageAvailable) item.append(element("p", "Browser storage is unavailable. Keep this page open; attachment reload recovery is unavailable.", "muted"));
      $("attachment-drafts").append(item);
    }
  }
  function matchingAttachmentReceipt(receipt, draft, kind) {
    const metadata = receipt && receipt.metadata;
    return receipt && receipt.version === 1 && receipt.kind === kind && receipt.room_id === draft.scope.room && validAttachmentID(receipt.object_id) && (draft.object_id === "" || receipt.object_id === draft.object_id) && metadata && metadata.id === receipt.object_id && ["name", "kind", "size", "sha256"].every(key => metadata[key] === draft.spec[key]) && receipt.state === { reserve: "reserved", verify: "verified", cancel: "abandoned" }[kind];
  }
  async function attachmentOperation(draft, action) {
    if (!validAttachmentDraft(draft) || busyAttachments.has(draft.id)) return;
    const generation = epoch, resolve = action.startsWith("resolve-"), kind = resolve ? action.slice(8) === "reservation" ? "reserve" : action.slice(8) === "verification" ? "verify" : "cancel" : action;
    const prior = { reserve: "prepared", verify: "uploaded", cancel: "cancel_ready" }[kind];
    const allowed = { reserve: ["prepared"], "resolve-reservation": ["reserving"], content: ["reserved", "uploading"], verify: ["uploaded"], "resolve-verification": ["verifying"], cancel: ["reserved", "uploading", "uploaded", "verified", "cancel_ready"], "resolve-cancellation": ["cancelling"] }[action];
    if (!allowed || !allowed.includes(draft.state) || action === "content" && !selectedFiles.has(draft.id)) return;
    const path = "/rooms/" + draft.scope.room + "/attachments", body = kind === "reserve" ? { client_id: draft.reserve_id, spec: draft.spec } : { client_id: kind === "verify" ? draft.verify_id : draft.cancel_id };
    if (!resolve) draft.state = { reserve: "reserving", content: "uploading", verify: "verifying", cancel: "cancelling" }[kind];
    busyAttachments.add(draft.id); saveAttachment(draft); renderAttachments();
    try {
      const result = await api(action === "reserve" ? path : action === "resolve-reservation" ? path + "/resolve-reservation" : path + "/" + draft.object_id + "/" + action, action === "content" ? selectedFiles.get(draft.id) : body, action === "content");
      if (generation !== epoch) return;
      if (resolve && result.data.outcome === "not_committed") { draft.state = prior; saveAttachment(draft); status("The original attachment action was not committed. The same recovery identity is available for explicit retry."); return; }
      if (action === "content" && result.ok && result.data.status === "uploaded" && result.data.object_id === draft.object_id && result.data.size === draft.spec.size && result.data.sha256 === draft.spec.sha256) { draft.state = "uploaded"; saveAttachment(draft); status("Bytes uploaded. Verify the attachment explicitly before including it in a message."); return; }
      if (result.ok && ["committed", "replayed"].includes(result.data.outcome) && matchingAttachmentReceipt(result.data.receipt, draft, kind)) {
        if (kind === "cancel") removeAttachmentDraft(draft.id);
        else { draft.object_id = result.data.receipt.object_id; draft.state = kind === "reserve" ? "reserved" : "verified"; saveAttachment(draft); }
        status("Attachment action confirmed: " + kind); return;
      }
      status("Attachment action is unconfirmed. Check the original reservation or verification; upload retry requires the exact original bytes.");
    } catch (error) { if (error !== stale && generation === epoch) status("Attachment result is uncertain. Its original metadata and recovery identity are retained."); }
    finally { if (generation === epoch) { busyAttachments.delete(draft.id); renderAttachments(); } }
  }
  const journalPrefix = "pairroom.enterprise.pending.v1:";
  const exactKeys = (value, allowed) => value && typeof value === "object" && !Array.isArray(value) && Object.keys(value).every((key) => allowed.includes(key));
  const knownValidationRefusal = (value) => exactKeys(value, ["http_status", "outcome", "class"]) && value.http_status === 400 && value.outcome === "refused" && value.class === "invalid_request";
  const revision = (value) => Number.isSafeInteger(value) && value >= 0;
  function journalKey(op) { return journalPrefix + JSON.stringify([op.scope.origin, op.scope.deployment, op.scope.user, op.scope.room, op.body.client_id]); }
  function operationResource(op) {
    if (op.path === "/rooms") return "form:create-room";
    const intent = op.body.intent || {};
    if (op.path === "/catalog/commands" && intent.kind === "project.create") return "form:create-project";
    if (op.scope.room) return "room:" + op.scope.room;
    if (op.path === "/catalog/commands") {
      if (intent.project_id) return "project:" + intent.project_id;
      if (intent.agent_id) return "agent:" + intent.agent_id;
      if (intent.session_id) return "session:" + intent.session_id;
    }
    if (op.path.startsWith("/devices/")) return "device:" + op.path.split("/")[2];
    if (op.path === "/administration/commands") return intent.target_user_id ? "administration:user:" + intent.target_user_id : "administration:identity:" + JSON.stringify(intent.identity);
    return op.path;
  }
  function resourceBlocked(resource) {
    const blocked = [...journalRecords.values()].find(op => operationResource(op) === resource);
    if (!blocked) return false;
    pending = blocked; renderJournal(); status("This Room or resource has a pending action. Check its original result; unrelated resources remain available."); return true;
  }
  function journalFits(op) {
    const key = journalKey(op), encoded = JSON.stringify(op);
    let count = 1, size = encoded.length;
    for (const [other, value] of journalRecords) if (other !== key) { count += 1; size += JSON.stringify(value).length; }
    return encoded.length <= 600000 && count <= 20 && size <= 1200000;
  }
  function validOperation(op) {
    if (!account || !op || op.version !== 1 || !op.scope || op.scope.origin !== window.location.origin || op.scope.deployment !== account.deployment_id || op.scope.user !== account.user_id || !op.body || !validID(op.body.client_id) || typeof op.label !== "string" || op.label.length > 100) return false;
    if (!exactKeys(op, ["version", "scope", "path", "resolvePath", "body", "label", "created_at", "validation_refusal"]) || !exactKeys(op.scope, ["origin", "deployment", "user", "room"]) || op.validation_refusal && !knownValidationRefusal(op.validation_refusal)) return false;
    const id = op.scope.room;
    if (id !== "" && !validID(id)) return false;
    const body = op.body;
    if (op.path === "/administration/commands" && op.resolvePath === "/administration/resolve" && id === "") {
      if (!exactKeys(body, ["client_id", "intent"]) || !exactKeys(body.intent, ["kind", "target_user_id", "identity", "enabled"]) || typeof body.intent.enabled !== "boolean") return false;
      const intent = body.intent;
      if (["user.status", "administrator.set"].includes(intent.kind)) return validID(intent.target_user_id) && intent.identity === undefined;
      return intent.kind === "identity.admission" && intent.target_user_id === undefined && exactKeys(intent.identity, ["connection_id", "issuer", "subject"]) && validID(intent.identity.connection_id) && typeof intent.identity.issuer === "string" && typeof intent.identity.subject === "string" && intent.identity.issuer.length > 0 && intent.identity.issuer.length <= 2048 && intent.identity.subject.length > 0 && intent.identity.subject.length <= 4096;
    }
    if (op.path === "/catalog/commands" && op.resolvePath === "/catalog/resolve") return exactKeys(body, ["client_id", "expected_revision", "intent"]) && revision(body.expected_revision) && exactKeys(body.intent, ["kind", "room_id", "project_id", "target_user_id", "session_id", "agent_id", "name"]) && (body.intent.room_id || "") === id;
    if (op.path === "/rooms" && id === body.room_id && op.resolvePath === "/rooms/" + id + "/resolve-create") return exactKeys(body, ["client_id", "room_id", "intent"]) && exactKeys(body.intent, ["name", "collaboration"]) && exactKeys(body.intent.collaboration, ["version", "mode", "instructions"]);
    if (id && op.path === "/rooms/" + id + "/messages" && op.resolvePath === "/rooms/" + id + "/resolve-message") return exactKeys(body, ["client_id", "to", "text", "attachment_ids"]) && ["slot1", "slot2"].includes(body.to) && typeof body.text === "string" && (body.attachment_ids === undefined || Array.isArray(body.attachment_ids) && body.attachment_ids.length <= 8 && new Set(body.attachment_ids).size === body.attachment_ids.length && body.attachment_ids.every(validAttachmentID));
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
    const key = journalKey(pending), busy = busyOperations.has(key);
    retryAllowed = absentOperations.has(key);
    const rejected = knownValidationRefusal(pending.validation_refusal);
    $("pending-title").textContent = rejected ? "Action rejected" : "Unconfirmed action";
    $("pending-guidance").textContent = rejected ? "The service rejected this input before applying it. Discard the record, then correct the input before preparing another action." : "Check the original result before trying the action again. Nothing is resent automatically.";
    $("pending-description").textContent = pending.label + (pending.scope.room ? " · " + pending.scope.room : "");
    $("retry-operation").hidden = !retryAllowed;
    $("resolve-operation").hidden = rejected;
    $("discard-operation").hidden = !retryAllowed && !rejected;
    $("discard-operation").textContent = rejected ? "Discard rejected action" : "Discard uncommitted action";
    $("resolve-operation").disabled = busy; $("retry-operation").disabled = busy; $("discard-operation").disabled = busy;
    $("journal-status").textContent = storageAvailable ? "This account's recovery record is saved in this browser." : "Browser storage is unavailable. Export this record before leaving; reload recovery is unavailable.";
  }
  function renderJournal() {
    $("pending-list").replaceChildren();
    if (pending) pending = journalRecords.get(journalKey(pending)) || null;
    if (!pending) pending = journalRecords.values().next().value || null;
    for (const op of journalRecords.values()) $("pending-list").append(button(op.label + " · " + operationResource(op), () => { pending = op; showPending(); }));
    if (!pending) $("pending-description").textContent = "";
    showPending();
  }
  function restoreJournal() {
    pending = null; retryAllowed = false; journalRecords.clear(); absentOperations.clear();
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
        journalRecords.set(key, op);
      }
    } catch { storageAvailable = false; }
    renderJournal();
  }
  async function runOperation(op, resolveOnly) {
    if (!validOperation(op)) return;
    const key = journalKey(op), generation = epoch;
    if (busyOperations.has(key) || !resolveOnly && knownValidationRefusal(op.validation_refusal)) return;
    if (!journalFits(op)) { status("Recovery storage is full. Resolve an existing action or shorten this input before submitting."); return; }
    journalRecords.set(key, op); absentOperations.delete(key); busyOperations.add(key);
    pending = op; persist(op); renderJournal(); renderAttachments();
    try {
      const body = resolveOnly && op.path === "/rooms" ? { client_id: op.body.client_id, intent: op.body.intent } : op.body;
      const result = await api(resolveOnly ? op.resolvePath : op.path, body);
      if (epoch !== generation) return;
      if (!resolveOnly && result.status === 400 && exactKeys(result.data, ["outcome", "class"]) && result.data.outcome === "refused" && result.data.class === "invalid_request") {
        op.validation_refusal = { http_status: 400, outcome: "refused", class: "invalid_request" };
        persist(op); status("The input was rejected before the action was applied. Discard this record and correct the input."); return;
      }
      if (resolveOnly && result.data.outcome === "not_committed") { absentOperations.add(key); status("The action was not committed. You may explicitly retry the same action."); return; }
      if (!result.ok || result.data.outcome === "unknown" || result.data.outcome && !["committed", "replayed"].includes(result.data.outcome)) { status("This action is not confirmed. Check its original result before taking another action."); return; }
      try { window.localStorage.removeItem(journalKey(op)); } catch { storageAvailable = false; }
      journalRecords.delete(key); absentOperations.delete(key); renderJournal(); status("Action confirmed: " + op.label + " · " + operationResource(op));
      if (op.body.attachment_ids) for (const [id, draft] of attachmentDrafts) if (draft.scope.room === op.scope.room && op.body.attachment_ids.includes(draft.object_id)) removeAttachmentDraft(id);
      if (op.scope.room && (!selectedRoomID || selectedRoomID === op.scope.room)) openRoom(op.scope.room);
      loadRooms();
      if (!$("projects-panel").hidden) { project = null; $("project-detail").hidden = true; loadProjects(); }
      if (!$("devices-panel").hidden) for (const resource of ["devices", "agents", "sessions"]) connections(resource);
      if (!$("governance-panel").hidden) loadGovernance();
      if (!$("administration-panel").hidden) loadAdministration();
    } catch (error) { if (error !== stale && epoch === generation) status("The result is uncertain: " + op.label + " · " + operationResource(op) + ". Its original recovery identity is retained."); }
    finally { if (epoch === generation) { busyOperations.delete(key); renderJournal(); renderAttachments(); } }
  }
  function mutate(path, resolvePath, body, label, roomID = "") {
    if (!account) return;
    const op = { version: 1, scope: { origin: window.location.origin, deployment: account.deployment_id, user: account.user_id, room: roomID }, path, resolvePath, body: { client_id: crypto.randomUUID(), ...body }, label, created_at: new Date().toISOString() };
    if (resourceBlocked(operationResource(op))) return;
    return runOperation(op, false);
  }
  function catalogCommand(intent, revision, label) {
    if (["project.create", "project.rename", "room.rename"].includes(intent.kind) && !validName(intent.name)) { status("Use a name of 1–160 UTF-8 bytes without control characters."); return; }
    return mutate("/catalog/commands", "/catalog/resolve", { expected_revision: revision, intent }, label, intent.room_id || "");
  }
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
  $("history-older").addEventListener("click", loadOlderHistory);
  for (const panel of ["rooms", "projects", "devices", "governance", "administration"]) $("tab-" + panel).addEventListener("click", () => { showPanel(panel); if (panel === "rooms") loadRooms(); if (panel === "projects") loadProjects(); if (panel === "devices") for (const resource of ["devices", "agents", "sessions"]) connections(resource); if (panel === "governance") loadGovernance(); if (panel === "administration") loadAdministration(); });
  $("administration-refresh").addEventListener("click", loadAdministration);
  for (const resource of ["users", "administrators", "admissions", "connections"]) $("administration-" + resource + "-more").addEventListener("click", () => administrationPage(resource, true));
  form("admission-form", () => administrationCommand({ kind: "identity.admission", identity: { connection_id: $("admission-connection").value, issuer: $("admission-issuer").value, subject: $("admission-subject").value }, enabled: $("admission-enabled").value === "true" }, "Set exact identity admission"));
  const actions = { "rooms-refresh": () => loadRooms(), "rooms-more": () => loadRooms(true), "projects-refresh": () => loadProjects(), "projects-more": () => loadProjects(true), "devices-refresh": () => { for (const value of ["devices", "agents", "sessions"]) connections(value); }, "devices-more": () => connections("devices", true), "agents-more": () => connections("agents", true), "sessions-more": () => connections("sessions", true), "room-members-more": () => loadRoomMembers(true), "project-members-more": () => loadProjectMembers(true), "room-refresh": () => room && openRoom(room.room_id), "binding-requests-refresh": loadBindings, "governance-refresh": loadGovernance, "project-archive": () => projectCommand("project.archive"), "project-restore": () => projectCommand("project.restore"), "archive-room": () => roomCommand("room.archive"), "restore-room": () => roomCommand("room.restore") };
  for (const [id, fn] of Object.entries(actions)) $(id).addEventListener("click", fn);
  $("rooms-state").addEventListener("change", () => loadRooms());
  $("collaboration-mode").addEventListener("change", () => { $("collaboration-instructions").disabled = $("collaboration-mode").value !== "custom"; });
  form("room-form", () => openRoom($("room-id").value.trim()));
  form("create-room-form", async () => {
    if (resourceBlocked("form:create-room")) return;
    if (!validName($("create-room-name").value.trim())) { status("Use a name of 1–160 UTF-8 bytes without control characters."); return; }
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
  form("send-form", () => {
    if (!room || $("send-message").disabled) return;
    const selected = [...attachmentDrafts.values()].filter(d => d.scope.room === room.room_id && d.state === "verified" && includedAttachments.has(d.id));
    if (!$("message-text").value && !selected.length) { status("Enter a message or include a verified attachment."); return; }
    if (selected.length > 8 || selected.reduce((n, d) => n + d.spec.size, 0) > 20 * 1024 * 1024) { status("A message supports eight attachments totaling at most 20 MiB."); return; }
    return mutate("/rooms/" + room.room_id + "/messages", "/rooms/" + room.room_id + "/resolve-message", { to: $("send-target").value, text: $("message-text").value, ...(selected.length ? { attachment_ids: selected.map(d => d.object_id) } : {}) }, "Send message", room.room_id);
  });
  form("attachment-form", async () => {
    if (!room || $("prepare-attachment").disabled) return;
    const file = $("attachment-file").files && $("attachment-file").files[0], roomID = room.room_id, version = roomVersion, generation = epoch;
    if (!file || file.size < 1 || file.size > 5 * 1024 * 1024 || attachmentDrafts.size >= 20 || [...attachmentDrafts.values()].filter(d => d.scope.room === roomID).length >= 8) { status("Choose a file of 1–5 MiB. Each Room supports eight draft attachments, with twenty drafts per account."); return; }
    try {
      const draft = { version: 1, id: crypto.randomUUID(), scope: { origin: window.location.origin, deployment: account.deployment_id, user: account.user_id, room: roomID }, spec: { name: file.name, kind: $("attachment-kind").value, size: file.size, sha256: await fileHash(file) }, state: "prepared", reserve_id: crypto.randomUUID(), verify_id: crypto.randomUUID(), cancel_id: crypto.randomUUID(), object_id: "" };
      if (generation !== epoch || version !== roomVersion) return;
      if (!validAttachmentDraft(draft)) throw new Error("invalid metadata");
      selectedFiles.set(draft.id, file); saveAttachment(draft); $("attachment-file").value = ""; renderAttachments(); status("Attachment metadata prepared locally. Reserve, upload and verify each remain explicit.");
    } catch { if (generation === epoch) status("The attachment could not be prepared. Use a safe filename and select file or image explicitly."); }
  });
  form("governance-form", async () => {
    if (!account.administrator) return;
    const generation = epoch, intent = { room_id: $("governance-room").value.trim(), expected_owner_user_id: $("governance-owner").value.trim(), target_user_id: $("governance-target").value.trim(), expected_revision: Number($("governance-revision").value) };
    if (!Number.isSafeInteger(intent.expected_revision) || intent.expected_revision < 1) return;
    if (resourceBlocked("room:" + intent.room_id)) return;
    try {
      const result = await api("/governance/actions", { client_id: crypto.randomUUID(), intent });
      if (generation !== epoch || !result.ok) return;
      if (result.data.authorization_url) { const url = new URL(result.data.authorization_url); if (url.protocol !== "https:" || url.username || url.password) throw new Error("invalid provider URL"); window.location.assign(url.href); } else loadGovernance();
    } catch (error) { if (error !== stale && generation === epoch) status("Ownership verification could not be started."); }
  });
  $("resolve-operation").addEventListener("click", () => pending && runOperation(pending, true));
  $("retry-operation").addEventListener("click", () => pending && retryAllowed && runOperation(pending, false));
  $("discard-operation").addEventListener("click", () => {
    if (!pending || busyOperations.has(journalKey(pending)) || !retryAllowed && !knownValidationRefusal(pending.validation_refusal)) return;
    const key = journalKey(pending);
    try { window.localStorage.removeItem(key); } catch { storageAvailable = false; }
    journalRecords.delete(key); absentOperations.delete(key); pending = null; retryAllowed = false; renderJournal(); status("The rejected or uncommitted action record was discarded. Correct the input before preparing a new action.");
  });
  $("import-operation").addEventListener("change", async () => {
    const generation = epoch, file = $("import-operation").files && $("import-operation").files[0];
    if (!account || !file || file.size > 600000) return;
    try {
      const encoded = await file.text();
      if (generation !== epoch) return;
      const op = JSON.parse(encoded);
      if (validAttachmentDraft(op)) {
        const existing = attachmentDrafts.get(op.id);
        if (existing) { status("The original attachment recovery metadata is already loaded."); return; }
        if (attachmentDrafts.size >= 20) throw new Error("drafts full");
        saveAttachment(op); renderAttachments(); status("Attachment recovery metadata loaded. Original result checks and uploads remain explicit."); return;
      }
      if (!validOperation(op)) throw new Error("wrong scope");
      const key = journalKey(op);
      if (journalRecords.has(key)) { pending = journalRecords.get(key); renderJournal(); status("The original recovery record is already loaded."); return; }
      if (!journalFits(op)) throw new Error("journal full");
      journalRecords.set(key, op); pending = op; retryAllowed = false; persist(op); renderJournal(); status("Recovery record loaded. Check the original result before retrying.");
    } catch { if (generation === epoch) status("This recovery record is invalid or belongs to another account or deployment."); }
    finally { if (generation === epoch) $("import-operation").value = ""; }
  });
  function exportRecord(record, filename) {
    const url = URL.createObjectURL(new Blob([JSON.stringify(record, null, 2)], { type: "application/json" })), link = element("a");
    link.href = url; link.download = filename; link.click(); URL.revokeObjectURL(url);
  }
  $("export-operation").addEventListener("click", () => pending && exportRecord(pending, "pairroom-pending-" + pending.body.client_id + ".json"));
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
