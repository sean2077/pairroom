(() => {
  "use strict";
  const byId = (id) => document.getElementById(id);
  let csrf = "";
  let userID = "";
  let pendingLogout = null;
  let accountEpoch = 0;
  let roomRequest = 0;
  let roomController = null;
  const inFlight = new Set();

  function clearRoom() {
    byId("room").hidden = true;
    byId("messages").replaceChildren();
    byId("room-title").textContent = "Room";
    byId("room-sequence").textContent = "";
  }

  function invalidatePrivate() {
    accountEpoch += 1;
    roomRequest += 1;
    for (const controller of inFlight) controller.abort();
    inFlight.clear();
    roomController = null;
    csrf = "";
    userID = "";
    clearRoom();
    byId("sign-out").hidden = true;
    byId("room-id").disabled = true;
    byId("room-id").value = "";
    byId("open-room").disabled = true;
    byId("status").textContent = "";
    return accountEpoch;
  }

  function startRequest() {
    const controller = new AbortController();
    inFlight.add(controller);
    return controller;
  }

  function current(epoch, controller) {
    return accountEpoch === epoch && !controller.signal.aborted;
  }

  function signedOut(message) {
    invalidatePrivate();
    pendingLogout = null;
    byId("account-state").textContent = message;
    byId("sign-in").hidden = false;
    byId("refresh").disabled = false;
    byId("sign-out").disabled = false;
  }

  async function refreshAccount() {
    const epoch = invalidatePrivate();
    const controller = startRequest();
    byId("account-state").textContent = "Checking your session…";
    byId("sign-in").hidden = true;
    byId("refresh").disabled = true;
    try {
      const response = await fetch("/enterprise/api/v1/session", { cache: "no-store", credentials: "same-origin", signal: controller.signal });
      if (!current(epoch, controller)) return;
      if (response.status === 401) {
        signedOut("Sign in to view your Rooms.");
        return;
      }
      if (!response.ok) throw new Error("unavailable");
      const session = await response.json();
      if (!current(epoch, controller)) return;
      if (typeof session.user_id !== "string" || !session.user_id || typeof session.csrf !== "string" || !session.csrf) throw new Error("invalid session");
      if (pendingLogout && pendingLogout.csrf !== session.csrf) pendingLogout = null;
      userID = session.user_id;
      csrf = session.csrf;
      byId("account-state").textContent = session.administrator ? "Signed in · Organization administrator" : "Signed in";
      byId("sign-in").hidden = true;
      byId("sign-out").hidden = false;
      byId("room-id").disabled = false;
      byId("open-room").disabled = false;
    } catch {
      if (current(epoch, controller)) signedOut("Your session could not be checked. Try refreshing your account.");
    } finally {
      inFlight.delete(controller);
      if (accountEpoch === epoch) byId("refresh").disabled = false;
    }
  }

  byId("refresh").addEventListener("click", refreshAccount);
  byId("room-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!csrf || !userID) return;
    const roomID = byId("room-id").value.trim();
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(roomID)) {
      byId("status").textContent = "Enter a valid Room ID.";
      return;
    }
    const epoch = accountEpoch;
    const expectedUser = userID;
    const requestID = ++roomRequest;
    if (roomController) roomController.abort();
    const controller = startRequest();
    roomController = controller;
    const isCurrent = () => current(epoch, controller) && userID === expectedUser && roomRequest === requestID;
    clearRoom();
    byId("open-room").disabled = true;
    byId("status").textContent = "Opening Room…";
    try {
      const response = await fetch(`/enterprise/api/v1/rooms/${encodeURIComponent(roomID)}`, { cache: "no-store", credentials: "same-origin", signal: controller.signal });
      if (!isCurrent()) return;
      if (response.status === 401) {
        signedOut("Your session has ended. Sign in again.");
        byId("status").textContent = "";
        return;
      }
      if (!response.ok) throw new Error("unavailable");
      const room = await response.json();
      if (!isCurrent()) return;
      const messages = Array.isArray(room.messages) ? room.messages : [];
      byId("room-title").textContent = room.room_id;
      byId("room-sequence").textContent = `${messages.length} messages`;
      for (const message of messages) {
        const item = document.createElement("li");
        const label = document.createElement("p");
        label.className = "message-label";
        label.textContent = `${message.from} → ${message.to} · ${message.state}`;
        const text = document.createElement("p");
        text.className = "message-text";
        text.textContent = message.text;
        item.append(label, text);
        byId("messages").append(item);
      }
      byId("room").hidden = false;
      byId("status").textContent = messages.length ? "" : "This Room has no messages yet.";
    } catch {
      if (isCurrent()) byId("status").textContent = "This Room is unavailable for your account. Check the Room ID or try again later.";
    } finally {
      inFlight.delete(controller);
      if (isCurrent()) byId("open-room").disabled = !csrf;
    }
  });

  byId("sign-out").addEventListener("click", async () => {
    if (!csrf) return;
    if (!pendingLogout || pendingLogout.csrf !== csrf) pendingLogout = { id: crypto.randomUUID(), csrf };
    const operation = pendingLogout;
    const epoch = invalidatePrivate();
    const controller = startRequest();
    byId("account-state").textContent = "Signing out…";
    byId("sign-out").disabled = true;
    try {
      const response = await fetch("/enterprise/api/v1/session/logout", {
        method: "POST", credentials: "same-origin", cache: "no-store", signal: controller.signal,
        headers: { "Content-Type": "application/json", "X-PairRoom-CSRF": operation.csrf },
        body: JSON.stringify({ client_id: operation.id })
      });
      if (!current(epoch, controller)) return;
      if (response.status === 401) {
        signedOut("Your session has ended.");
        return;
      }
      if (!response.ok) throw new Error("unconfirmed");
      signedOut("You are signed out.");
      byId("status").textContent = "";
    } catch {
      if (current(epoch, controller)) byId("status").textContent = "Sign-out could not be confirmed. Refresh your account status before trying again.";
    } finally {
      inFlight.delete(controller);
      if (accountEpoch === epoch) byId("sign-out").disabled = false;
    }
  });

  refreshAccount();
})();
