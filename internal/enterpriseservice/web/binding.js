(() => {
  "use strict";
  const byId = (id) => document.getElementById(id);
  const idPattern = "([A-Za-z0-9][A-Za-z0-9._-]{0,127})";
  const match = window.location.pathname.match(new RegExp(`^/enterprise/binding-requests/${idPattern}/${idPattern}$`));
  const roomID = match ? match[1] : "";
  const requestID = match ? match[2] : "";
  const base = `/enterprise/api/v1/rooms/${roomID}/binding-requests`;
  let epoch = 0;
  let csrf = "";
  let info = null;
  let pendingDecision = null;
  let observedSession = null;
  const accountChannel = typeof BroadcastChannel === "function" ? new BroadcastChannel("pairroom.enterprise.account.v1") : null;
  const inFlight = new Set();
  const privateFields = ["binding-room", "binding-user", "binding-device", "binding-agent", "binding-runtime", "binding-slot", "binding-park", "binding-expiry", "binding-replacement", "binding-request-state", "binding-pending", "binding-status"];

  function invalidate() {
    epoch += 1;
    for (const controller of inFlight) controller.abort();
    inFlight.clear();
    csrf = "";
    info = null;
    byId("binding-details").hidden = true;
    byId("binding-approve").disabled = true;
    byId("binding-deny").disabled = true;
    byId("binding-resolve").hidden = true;
    byId("binding-resolve").disabled = true;
    for (const id of privateFields) byId(id).textContent = "";
    return epoch;
  }

  function controller() { const value = new AbortController(); inFlight.add(value); return value; }
  const current = (generation, request) => epoch === generation && !request.signal.aborted;
  const fetchOptions = (request) => ({ credentials: "same-origin", cache: "no-store", redirect: "error", signal: request.signal });
  const matchesSession = (response, session) => session && response.headers && response.headers.get("X-PairRoom-User") === session.user_id && response.headers.get("X-PairRoom-Session") === session.session_id;
  if (accountChannel) accountChannel.onmessage = ({ data }) => {
    if (!data || typeof data.deployment !== "string" || typeof data.user !== "string" || typeof data.session !== "string") return;
    if (observedSession && (data.deployment !== observedSession.deployment_id || data.user === observedSession.user_id && data.session === observedSession.session_id)) return;
    invalidate(); observedSession = null; pendingDecision = null;
    byId("binding-account").textContent = "Account changed in another tab. Refresh this request before deciding.";
    byId("binding-refresh").disabled = false;
  };

  async function refresh() {
    const generation = invalidate();
    const request = controller();
    byId("binding-refresh").disabled = true;
    byId("binding-sign-in").hidden = true;
    byId("binding-account").textContent = "Checking your session…";
    try {
      if (!roomID || !requestID) throw new Error("invalid request");
      const sessionResponse = await fetch("/enterprise/api/v1/session", fetchOptions(request));
      if (!current(generation, request)) return;
      if (sessionResponse.status === 401) {
        pendingDecision = null;
        byId("binding-account").textContent = "Sign in as the Room Owner, then return here and refresh.";
        byId("binding-sign-in").hidden = false;
        return;
      }
      if (!sessionResponse.ok) throw new Error("unavailable");
      const session = await sessionResponse.json();
      if (!current(generation, request)) return;
      if (!session.user_id || !session.csrf || !session.session_id || !session.deployment_id || !matchesSession(sessionResponse, session)) throw new Error("invalid session");
      observedSession = { user_id: session.user_id, session_id: session.session_id, deployment_id: session.deployment_id };
      csrf = session.csrf;
      if (pendingDecision && pendingDecision.csrf !== csrf) pendingDecision = null;
      if (pendingDecision) {
        byId("binding-resolve").hidden = false;
        byId("binding-resolve").disabled = false;
      }
      byId("binding-account").textContent = `Account: ${session.user_id}`;

      const listResponse = await fetch(base, fetchOptions(request));
      if (!current(generation, request)) return;
      if (!listResponse.ok || !matchesSession(listResponse, session)) throw new Error("owner required");
      const list = await listResponse.json();
      if (!current(generation, request)) return;
      const response = await fetch(`${base}/${requestID}`, fetchOptions(request));
      if (!current(generation, request)) return;
      if (!response.ok || !matchesSession(response, session)) throw new Error("unavailable");
      const selected = await response.json();
      if (!current(generation, request)) return;
      const contextResponse = await fetch(`/enterprise/api/v1/rooms/${roomID}/binding-context`, fetchOptions(request));
      if (!current(generation, request)) return;
      if (!contextResponse.ok || !matchesSession(contextResponse, session)) throw new Error("unavailable");
      const context = await contextResponse.json();
      if (!current(generation, request)) return;
      const value = selected.request;
      if (!value || value.request_id !== requestID || value.room_id !== roomID || context.room_id !== roomID || context.owner_user_id !== session.user_id) throw new Error("wrong request");
      const slot = context.slots && context.slots[value.slot];
      const sameGeneration = slot && Number.isSafeInteger(value.observed_generation) && slot.head_generation === value.observed_generation;
      info = value;
      byId("binding-room").textContent = value.room_id;
      byId("binding-user").textContent = value.user_id;
      byId("binding-device").textContent = value.device_id;
      byId("binding-agent").textContent = value.agent_id;
      byId("binding-runtime").textContent = value.runtime;
      byId("binding-slot").textContent = `${value.slot} · generation ${value.observed_generation}`;
      byId("binding-park").textContent = value.park_enabled ? "Enabled by the endpoint" : "Disabled by the endpoint";
      byId("binding-expiry").textContent = new Date(value.expires_at).toLocaleString();
      byId("binding-replacement").textContent = value.replace ? "This replaces the existing Binding. Accepted work stays in history; the previous native session is not stopped." : "This request joins a vacant slot.";
      byId("binding-request-state").textContent = context.ownership_pending ? "Ownership must be recovered before approval." : !sameGeneration ? "The slot has changed. Ask the endpoint to submit a new request." : `Request: ${value.status}`;
      byId("binding-pending").textContent = `${Array.isArray(list.requests) ? list.requests.length : 0} pending requests in this Room.`;
      const actionable = !pendingDecision && (value.status === "pending" || value.status === "approved");
      byId("binding-approve").disabled = !actionable || !sameGeneration || context.ownership_pending;
      byId("binding-deny").disabled = !actionable || context.ownership_pending;
      byId("binding-details").hidden = false;
    } catch {
      if (current(generation, request)) {
        invalidate();
        byId("binding-account").textContent = "Request unavailable for this account.";
        byId("binding-status").textContent = "Only the current Room Owner can review and approve this request.";
        byId("binding-sign-in").hidden = false;
        byId("binding-refresh").disabled = false;
      }
    } finally {
      inFlight.delete(request);
      if (epoch === generation) byId("binding-refresh").disabled = false;
    }
  }

  async function decide(action) {
    if (!csrf || !info || byId(`binding-${action === "approve" ? "approve" : "deny"}`).disabled) return;
    const proof = csrf;
    const operation = { client_id: crypto.randomUUID(), action };
    // An uncertain decision can only be inspected with its original operation.
    // This state is page-local and discarded when the browser session changes.
    pendingDecision = { csrf: proof, operation };
    await submitDecision(operation, proof, false);
  }

  async function resolve() {
    if (!pendingDecision || byId("binding-resolve").disabled) return;
    await submitDecision(pendingDecision.operation, pendingDecision.csrf, true);
  }

  async function submitDecision(operation, proof, resolveOnly) {
    const expectedSession = observedSession;
    const generation = invalidate();
    const request = controller();
    byId("binding-status").textContent = resolveOnly ? "Checking the original decision…" : operation.action === "approve" ? "Approving the endpoint's exact request…" : "Denying request…";
    try {
      const response = await fetch(`${base}/${requestID}/${resolveOnly ? "resolve-decision" : "decision"}`, {
        method: "POST", ...fetchOptions(request),
        headers: { "Content-Type": "application/json", "X-PairRoom-CSRF": proof },
        body: JSON.stringify(operation)
      });
      if (!current(generation, request)) return;
      if (!matchesSession(response, expectedSession)) throw new Error("different session");
      const result = await response.json();
      if (!current(generation, request)) return;
      if (resolveOnly && response.status === 409 && result.outcome === "not_committed") {
        pendingDecision = null;
        byId("binding-status").textContent = "The original decision was not committed. Refresh the request before making a new decision.";
        return;
      }
      if (!response.ok) throw new Error("unconfirmed");
      if (!["committed", "replayed"].includes(result.outcome) || !result.receipt || result.receipt.kind !== "decide" || !result.receipt.request || result.receipt.request.room_id !== roomID || result.receipt.request.request_id !== requestID || result.receipt.request.status !== (operation.action === "approve" ? "approved" : "denied")) throw new Error("wrong receipt");
      pendingDecision = null;
      byId("binding-status").textContent = operation.action === "approve" ? "Approved. The original endpoint must now complete its connection with its private proof." : "Request denied.";
    } catch {
      if (current(generation, request)) {
        byId("binding-status").textContent = "The decision could not be confirmed. Check the original decision before taking another action.";
        byId("binding-resolve").hidden = false;
        byId("binding-resolve").disabled = false;
      }
    } finally { inFlight.delete(request); }
  }

  byId("binding-refresh").addEventListener("click", refresh);
  byId("binding-approve").addEventListener("click", () => decide("approve"));
  byId("binding-deny").addEventListener("click", () => decide("deny"));
  byId("binding-resolve").addEventListener("click", resolve);
  refresh();
})();
