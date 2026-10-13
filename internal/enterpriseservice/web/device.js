(() => {
  "use strict";
  const byId = (id) => document.getElementById(id);
  const match = window.location.pathname.match(/^\/enterprise\/device-enrollments\/([A-Za-z0-9][A-Za-z0-9._-]{0,127})$/);
  const requestID = match ? match[1] : "";
  let epoch = 0;
  let csrf = "";
  let info = null;
  let observedSession = null;
  const accountChannel = typeof BroadcastChannel === "function" ? new BroadcastChannel("pairroom.enterprise.account.v1") : null;
  const inFlight = new Set();

  function invalidate() {
    epoch += 1;
    for (const controller of inFlight) controller.abort();
    inFlight.clear();
    csrf = "";
    info = null;
    byId("device-code").value = "";
    byId("device-confirmation").hidden = true;
    byId("device-details").hidden = true;
    byId("device-approve").disabled = true;
    byId("device-deny").disabled = true;
    for (const id of ["device-label", "device-origin", "device-id", "device-fingerprint", "device-expiry", "device-request-state", "device-status"]) byId(id).textContent = "";
    return epoch;
  }

  function requestController() {
    const controller = new AbortController();
    inFlight.add(controller);
    return controller;
  }

  const current = (generation, controller) => epoch === generation && !controller.signal.aborted;
  const matchesSession = (response, session) => session && response.headers && response.headers.get("X-PairRoom-User") === session.user_id && response.headers.get("X-PairRoom-Session") === session.session_id;
  if (accountChannel) accountChannel.onmessage = ({ data }) => {
    if (!data || typeof data.deployment !== "string" || typeof data.user !== "string" || typeof data.session !== "string") return;
    if (observedSession && (data.deployment !== observedSession.deployment_id || data.user === observedSession.user_id && data.session === observedSession.session_id)) return;
    invalidate(); observedSession = null;
    byId("device-account").textContent = "Account changed in another tab. Refresh this request before approving.";
    byId("device-refresh").disabled = false;
  };

  async function refresh() {
    const generation = invalidate();
    const controller = requestController();
    byId("device-refresh").disabled = true;
    byId("device-account").textContent = "Checking your session…";
    byId("device-sign-in").hidden = true;
    try {
      if (!requestID) throw new Error("invalid request");
      const sessionResponse = await fetch("/enterprise/api/v1/session", { credentials: "same-origin", cache: "no-store", signal: controller.signal });
      if (!current(generation, controller)) return;
      if (sessionResponse.status === 401) {
        byId("device-account").textContent = "Sign in, then return here and refresh the request.";
        byId("device-sign-in").hidden = false;
        return;
      }
      if (!sessionResponse.ok) throw new Error("unavailable");
      const session = await sessionResponse.json();
      if (!current(generation, controller)) return;
      if (typeof session.csrf !== "string" || !session.csrf || !session.user_id || !session.session_id || !session.deployment_id || !matchesSession(sessionResponse, session)) throw new Error("invalid session");
      observedSession = { user_id: session.user_id, session_id: session.session_id, deployment_id: session.deployment_id };
      csrf = session.csrf;
      byId("device-account").textContent = `Account: ${session.user_id}`;
      const response = await fetch(`/enterprise/api/v1/device-enrollments/${requestID}`, { credentials: "same-origin", cache: "no-store", signal: controller.signal });
      if (!current(generation, controller)) return;
      if (!response.ok || !matchesSession(response, session)) throw new Error("unavailable");
      const request = await response.json();
      if (!current(generation, controller)) return;
      const mode = request.transport_mode || "loopback";
      if (request.request_id !== requestID || !["loopback", "headless"].includes(mode) || mode === "headless" && !/^[a-f0-9]{32}$/.test(request.candidate_fingerprint || "")) throw new Error("wrong request");
      info = request;
      byId("device-label").textContent = request.label;
      byId("device-origin").textContent = request.origin;
      byId("device-id").textContent = request.device_id;
      byId("device-fingerprint").textContent = request.candidate_fingerprint || "—";
      byId("device-confirmation").hidden = mode !== "headless";
      byId("device-expiry").textContent = new Date(request.expires_at).toLocaleString();
      byId("device-request-state").textContent = request.status === "approved" ? mode === "headless" ? "Approved. Your original CLI can complete enrollment until this request expires." : "Previously approved. Approve again only if your CLI did not receive the completion." : `Request: ${request.status}`;
      const pending = request.status === "pending" || request.status === "approved" && mode === "loopback";
      byId("device-approve").textContent = request.status === "approved" && mode === "loopback" ? "Approve again" : "Approve device";
      byId("device-approve").disabled = !pending;
      byId("device-deny").disabled = !["pending", "approved"].includes(request.status);
      byId("device-details").hidden = false;
    } catch {
      if (current(generation, controller)) {
        invalidate();
        byId("device-account").textContent = "Request unavailable for this account.";
        byId("device-status").textContent = "Check the request or sign in to the intended account, then refresh.";
        byId("device-sign-in").hidden = false;
        byId("device-refresh").disabled = false;
      }
    } finally {
      inFlight.delete(controller);
      if (epoch === generation) byId("device-refresh").disabled = false;
    }
  }

  function validCompletionLocation(value) {
    try {
      const location = new URL(value);
      const host = location.hostname;
      const loopback = host === "[::1]" || /^127\.(?:[0-9]{1,3}\.){2}[0-9]{1,3}$/.test(host);
      const explicitPort = /^http:\/\/(?:127\.(?:[0-9]{1,3}\.){2}[0-9]{1,3}|\[::1\]):([1-9][0-9]{0,4})\//.exec(value);
      return location.protocol === "http:" && loopback && explicitPort && Number(explicitPort[1]) <= 65535 && !location.username && !location.password && !location.hash && [...location.searchParams.keys()].length === 2 && /^[A-Za-z0-9_-]{43}$/.test(location.searchParams.get("completion_code") || "") && /^[A-Za-z0-9_-]{43}$/.test(location.searchParams.get("state") || "");
    } catch { return false; }
  }

  async function decide(action) {
    if (!csrf || !info || !["pending", "approved"].includes(info.status)) return;
    const headless = info.transport_mode === "headless";
    let confirmationCode = "";
    if (headless && action === "approve") {
      confirmationCode = byId("device-code").value;
      if (!/^[0-9ABCDEFGHJKMNPQRSTVWXYZ]{4}-?[0-9ABCDEFGHJKMNPQRSTVWXYZ]{4}$/.test(confirmationCode)) { byId("device-status").textContent = "Enter the eight uppercase symbols shown by your own CLI."; return; }
    }
    const proof = csrf;
    const expectedSession = observedSession;
    const generation = invalidate();
    const controller = requestController();
    byId("device-status").textContent = action === "approve" ? "Approving device…" : "Denying request…";
    try {
      const response = await fetch(`/enterprise/api/v1/device-enrollments/${requestID}/decision`, {
        method: "POST", credentials: "same-origin", cache: "no-store", redirect: "error", signal: controller.signal,
        headers: { "Content-Type": "application/json", "X-PairRoom-CSRF": proof },
        body: JSON.stringify({ client_id: crypto.randomUUID(), action, ...(confirmationCode ? { confirmation_code: confirmationCode } : {}) })
      });
      if (!current(generation, controller)) return;
      if (!matchesSession(response, expectedSession)) throw new Error("unconfirmed");
      const result = await response.json();
      if (!current(generation, controller)) return;
      if (result.request_id !== requestID) throw new Error("wrong request");
      if (headless) {
        if (!["committed", "replayed"].includes(result.outcome) || result.kind !== "device.enrollment.headless.decide" || result.transport_mode !== "headless" || result.completion_location) throw new Error("unconfirmed");
        if (response.status === 400 && result.disposition === "confirmation_rejected") { byId("device-status").textContent = "The code did not match. This failed confirmation was recorded. Refresh the request before entering your own CLI code again."; return; }
        if (response.status === 429 && result.disposition === "verification_exhausted") { byId("device-status").textContent = "The confirmation limit was reached and this request was invalidated. No Device credential was issued. Start a new request explicitly in your CLI."; return; }
        if (!response.ok || result.disposition || result.status !== (action === "approve" ? "approved" : "denied")) throw new Error("unconfirmed");
        byId("device-status").textContent = action === "approve" ? "Device approved. Return to your original CLI to complete enrollment. Room membership, content access and Binding approval remain separate." : "Request denied. No Device credential was issued.";
        return;
      }
      if (!response.ok) throw new Error("unconfirmed");
      if (action === "approve") {
        if (!validCompletionLocation(result.completion_location)) throw new Error("invalid completion");
        window.location.assign(result.completion_location);
      } else {
        byId("device-status").textContent = "Request denied. No device credential was issued.";
      }
    } catch {
      if (current(generation, controller)) byId("device-status").textContent = "The decision could not be confirmed. Refresh the request before explicitly deciding again.";
    } finally {
      inFlight.delete(controller);
    }
  }

  byId("device-refresh").addEventListener("click", refresh);
  byId("device-approve").addEventListener("click", () => decide("approve"));
  byId("device-deny").addEventListener("click", () => decide("deny"));
  refresh();
})();
