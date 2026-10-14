package enterpriseservice

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

// Actual TLS/HTTP + maintained OIDC verification + PostgreSQL, using the
// existing synthetic IdP fixture. Endpoint secrets are retained by this test
// client before requests; this is not a vendor-session or browser E2E claim.
type grantsPGDevice struct{ deviceID, grantID, access, refresh, proof string }
type grantsPGBinding struct {
	bindingID, grantID, token string
	generation                uint64
	slot                      model.ActorID
}

func grantsPGNative(t *testing.T, f *httpPGFixture) *http.Client {
	t.Helper()
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.roots.Clone(), MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func grantsPGCall(t *testing.T, f *httpPGFixture, client *http.Client, method, path, bearer string, value any) (int, []byte) {
	t.Helper()
	var body []byte
	var err error
	if value != nil {
		body, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := http.NewRequest(method, f.origin+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(DeviceDeploymentHeader, f.cfg.DeploymentID)
	if value != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Location") != "" {
		t.Fatal("native API redirected a credential-bearing request")
	}
	return response.StatusCode, data
}

func grantsPGHash(secret string) string {
	hash := tokenHash(secret)
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func grantsPGEnroll(t *testing.T, f *httpPGFixture, native, browser *http.Client, csrf, suffix string) grantsPGDevice {
	t.Helper()
	access, _ := enterpriseauth.NewBrowserSecret()
	refresh, _ := enterpriseauth.NewBrowserSecret()
	proof, _ := enterpriseauth.NewBrowserSecret()
	state, _ := enterpriseauth.NewBrowserSecret()
	device := grantsPGDevice{deviceID: "device-" + suffix, grantID: "device-grant-" + suffix, access: access, refresh: refresh, proof: proof}
	begin := deviceBeginRequest{RequestID: "enrollment-" + suffix, DeviceID: device.deviceID, Label: "Synthetic endpoint " + suffix, CallbackURI: "http://127.0.0.1:49152/pairroom/callback", ClientState: privateDeviceInput(state), ProofHash: grantsPGHash(proof), Credential: deviceCandidateRequest{GrantID: device.grantID, AccessHash: grantsPGHash(access), RefreshHash: grantsPGHash(refresh)}}
	status, body := grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/enrollments", "", begin)
	if status != 200 || !bytes.Contains(body, []byte("verification_url")) || bytes.Contains(body, []byte(proof)) || bytes.Contains(body, []byte(access)) {
		t.Fatalf("real enrollment begin: %d %s", status, body)
	}
	status, _ = grantsPGCall(t, f, native, http.MethodGet, "/enterprise/device/v1/session", access, nil)
	if status != 401 {
		t.Fatal("pending enrollment activated a Device credential before browser consent")
	}
	status, body = f.post(browser, "/enterprise/api/v1/device-enrollments/"+begin.RequestID+"/decision", map[string]string{"client_id": "approve-" + suffix, "action": "approve"}, csrf)
	if status != 200 {
		t.Fatalf("real browser approval: %d %s", status, body)
	}
	var decision struct {
		CompletionLocation string `json:"completion_location"`
	}
	if err := json.Unmarshal(body, &decision); err != nil {
		t.Fatal(err)
	}
	location, err := url.Parse(decision.CompletionLocation)
	if err != nil {
		t.Fatal(err)
	}
	if location.Scheme != "http" || location.Host != "127.0.0.1:49152" || location.Query().Get("state") != state {
		t.Fatal("browser approval changed client-owned callback/state")
	}
	code := location.Query().Get("completion_code")
	wrongProof, _ := enterpriseauth.NewBrowserSecret()
	complete := deviceCompleteRequest{ClientID: "complete-" + suffix, ClientProof: privateDeviceInput(wrongProof), CompletionCode: privateDeviceInput(code)}
	status, _ = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/enrollments/"+begin.RequestID+"/complete", "", complete)
	if status < 400 {
		t.Fatal("approved enrollment completed without the original native proof")
	}
	complete.ClientProof = privateDeviceInput(proof)
	status, body = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/enrollments/"+begin.RequestID+"/complete", "", complete)
	if status != 200 || !bytes.Contains(body, []byte(device.grantID)) || bytes.Contains(body, []byte(access)) || bytes.Contains(body, []byte(refresh)) {
		t.Fatalf("real native completion: %d %s", status, body)
	}
	status, body = grantsPGCall(t, f, native, http.MethodGet, "/enterprise/device/v1/session", access, nil)
	if status != 200 || !bytes.Contains(body, []byte(device.deviceID)) || !bytes.Contains(body, []byte(device.grantID)) {
		t.Fatalf("current Device inspection: %d %s", status, body)
	}
	return device
}

func grantsPGBind(t *testing.T, f *httpPGFixture, native, owner *http.Client, ownerCSRF, room string, device grantsPGDevice, slot model.ActorID, suffix string) grantsPGBinding {
	t.Helper()
	source, _ := enterpriseauth.NewBrowserSecret()
	status, body := grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/agents", device.access, map[string]string{"client_id": "agent-" + suffix, "source_key": source, "runtime": "codex"})
	if status != 200 {
		t.Fatalf("Agent registration: %d %s", status, body)
	}
	var agent struct {
		ID string `json:"agent_id"`
	}
	if err := json.Unmarshal(body, &agent); err != nil || agent.ID == "" {
		t.Fatal("Agent receipt omitted identity")
	}
	status, body = grantsPGCall(t, f, native, http.MethodGet, "/enterprise/device/v1/rooms/"+room+"/binding-context", device.access, nil)
	if status != 200 {
		t.Fatalf("Binding context: %d %s", status, body)
	}
	var view sqlstore.BindingContext
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	head, ok := view.Slots[slot]
	if !ok || !head.Vacant || head.HeadGeneration == 0 {
		t.Fatal("fixture expected actual vacant slot metadata")
	}
	proof, _ := enterpriseauth.NewBrowserSecret()
	token, _ := enterpriseauth.NewBrowserSecret()
	binding := grantsPGBinding{bindingID: "binding-" + suffix, grantID: "binding-grant-" + suffix, token: token, generation: head.HeadGeneration, slot: slot}
	request := bindingRequestBody{ClientID: "binding-request-op-" + suffix, RequestID: "binding-request-" + suffix, AgentID: agent.ID, BindingID: binding.bindingID, Slot: slot, ObservedGeneration: head.HeadGeneration, ParkEnabled: false, ProofHash: grantsPGHash(proof)}
	request.Grant.GrantID = binding.grantID
	request.Grant.TokenHash = grantsPGHash(token)
	base := "/enterprise/device/v1/rooms/" + room + "/binding-requests"
	status, body = grantsPGCall(t, f, native, http.MethodPost, base, device.access, request)
	if status != 200 {
		t.Fatalf("Binding request: %d %s", status, body)
	}
	completion := bindingCompletionBody{ClientID: "binding-complete-" + suffix, ClientProof: privateDeviceInput(proof)}
	status, _ = grantsPGCall(t, f, native, http.MethodPost, base+"/"+request.RequestID+"/complete", device.access, completion)
	if status < 400 {
		t.Fatal("Device consent alone substituted for Room Owner approval")
	}
	status, body = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/binding-requests/"+request.RequestID+"/decision", map[string]string{"client_id": "binding-approve-" + suffix, "action": "approve"}, ownerCSRF)
	if status != 200 {
		t.Fatalf("Owner Binding approval: %d %s", status, body)
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, base+"/"+request.RequestID+"/complete", device.access, completion)
	if status != 200 {
		t.Fatalf("original Device Binding completion: %d %s", status, body)
	}
	var receipt struct {
		Receipt sqlstore.BindingAuthorityReceipt `json:"receipt"`
	}
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Receipt.Binding == nil || receipt.Receipt.Binding.BindID != binding.bindingID || receipt.Receipt.Binding.Generation != head.HeadGeneration || receipt.Receipt.GrantID != binding.grantID || receipt.Receipt.Binding.ParkEnabled {
		t.Fatal("Binding acceptance changed original endpoint identity/generation/park intent")
	}
	if bytes.Contains(body, []byte(token)) || bytes.Contains(body, []byte(proof)) || bytes.Contains(body, []byte(request.Grant.TokenHash)) {
		t.Fatal("Binding receipt exposed a credential or proof")
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, base+"/"+request.RequestID+"/resolve-completion", device.access, completion)
	if status != 200 {
		t.Fatalf("accepted completion verification: %d %s", status, body)
	}
	return binding
}

func TestGrantsHTTPPGDeviceRotationAndBindingRenewalAcrossRealBoundaries(t *testing.T) {
	f := newHTTPPGFixture(t)
	native := grantsPGNative(t, f)
	owner := f.browser("member-one")
	f.login(owner)
	session := f.session(owner)
	status, body := grantsPGCall(t, f, native, http.MethodGet, "/enterprise/device/v1/deployment", "", nil)
	if status != 200 || !bytes.Contains(body, []byte(f.cfg.DeploymentID)) {
		t.Fatal("unauthenticated deployment preflight failed")
	}
	device := grantsPGEnroll(t, f, native, owner, session.CSRF, "one")
	nextAccess, _ := enterpriseauth.NewBrowserSecret()
	nextRefresh, _ := enterpriseauth.NewBrowserSecret()
	rotation := deviceRotationRequest{DeviceID: device.deviceID, CurrentGrantID: device.grantID, RotationID: "rotation-one", ClientProof: privateDeviceInput(device.proof), Next: deviceCandidateRequest{GrantID: "device-grant-one-next", AccessHash: grantsPGHash(nextAccess), RefreshHash: grantsPGHash(nextRefresh)}}
	status, body = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/credentials/rotate", device.refresh, rotation)
	if status != 200 {
		t.Fatalf("Device rotation: %d %s", status, body)
	}
	status, _ = grantsPGCall(t, f, native, http.MethodGet, "/enterprise/device/v1/session", device.access, nil)
	if status != 401 {
		t.Fatal("predecessor access credential remained usable after rotation")
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/credentials/recover", device.refresh, rotation)
	if status != 200 || !bytes.Contains(body, []byte(sqlstore.ControlDeviceRecover)) {
		t.Fatalf("one bounded predecessor recovery: %d %s", status, body)
	}
	status, _ = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/credentials/recover", device.refresh, rotation)
	if status < 400 {
		t.Fatal("immediate predecessor recovery was reusable")
	}
	device.access, device.refresh, device.grantID = nextAccess, nextRefresh, rotation.Next.GrantID
	status, _ = grantsPGCall(t, f, native, http.MethodGet, "/enterprise/device/v1/session", device.access, nil)
	if status != 200 {
		t.Fatal("pre-persisted current candidate could not prove rotation completion")
	}
	f.createRoom(owner, session.CSRF, "room-grants")
	binding := grantsPGBind(t, f, native, owner, session.CSRF, "room-grants", device, model.ActorSlot1, "one")
	newToken, _ := enterpriseauth.NewBrowserSecret()
	renew := bindingRenewalBody{ClientID: "narrow-renew-one", Generation: binding.generation}
	renew.Grant.GrantID = "binding-grant-one-next"
	renew.Grant.TokenHash = grantsPGHash(newToken)
	base := "/enterprise/device/v1/rooms/room-grants/bindings/" + binding.bindingID
	status, body = grantsPGCall(t, f, native, http.MethodPost, base+"/renew", device.access, renew)
	if status != 200 {
		t.Fatalf("narrow Binding renewal: %d %s", status, body)
	}
	var renewed struct {
		Receipt sqlstore.BindingAuthorityReceipt `json:"receipt"`
	}
	if err := json.Unmarshal(body, &renewed); err != nil {
		t.Fatal(err)
	}
	if renewed.Receipt.Kind != "renew" || renewed.Receipt.Request != nil || renewed.Receipt.Binding == nil || renewed.Receipt.Binding.BindID != binding.bindingID || renewed.Receipt.Binding.Generation != binding.generation || renewed.Receipt.GrantID != renew.Grant.GrantID {
		t.Fatal("narrow renewal replaced its Binding or generation")
	}
	status, _ = grantsPGCall(t, f, native, http.MethodPost, base+"/resolve-renewal", device.access, renew)
	if status != 200 {
		t.Fatal("renewal receipt could not be verified without another mutation")
	}
	status, _ = grantsPGCall(t, f, native, http.MethodGet, "/enterprise/device/v1/session", newToken, nil)
	if status != 401 {
		t.Fatal("narrow Binding token inherited Device authority")
	}
	status, _ = grantsPGCall(t, f, native, http.MethodGet, "/enterprise/api/v1/session", newToken, nil)
	if status < 400 {
		t.Fatal("narrow Binding token inherited browser/administrator authority")
	}
	intent := relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}
	request := struct {
		ClientID   string                          `json:"client_id"`
		BindingID  string                          `json:"binding_id"`
		Generation uint64                          `json:"generation"`
		Intent     relay.EnterpriseOperationIntent `json:"intent"`
	}{"claim-scope", binding.bindingID, binding.generation, intent}
	status, _ = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/room-grants/relay", device.access, request)
	if status < 400 {
		t.Fatal("ordinary Device token inherited narrow Relay authority")
	}
	status, _ = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/room-grants/relay", binding.token, request)
	if status < 400 {
		t.Fatal("renewed-away narrow token still authorized Relay")
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/room-grants/relay", newToken, request)
	if status != 200 {
		t.Fatalf("new narrow candidate did not preserve Binding authority: %d %s", status, body)
	}
}

type grantsPGRelayRequest struct {
	ClientID   string                          `json:"client_id"`
	BindingID  string                          `json:"binding_id"`
	Generation uint64                          `json:"generation"`
	Intent     relay.EnterpriseOperationIntent `json:"intent"`
}
type grantsPGRelayReply struct {
	Outcome sqlstore.Outcome                `json:"outcome"`
	Result  relay.EnterpriseOperationResult `json:"result"`
}

func TestGrantsHTTPPGTwoRemoteBindingsSendClaimAckAndResolveWithPrivateReceipt(t *testing.T) {
	f := newHTTPPGFixture(t)
	native := grantsPGNative(t, f)
	owner := f.browser("member-one")
	member := f.browser("member-two")
	f.login(owner)
	f.login(member)
	ownerSession, memberSession := f.session(owner), f.session(member)
	f.createRoom(owner, ownerSession.CSRF, "room-two-endpoints")
	status, body := f.post(owner, "/enterprise/api/v1/rooms/room-two-endpoints/memberships", map[string]any{"client_id": "admit-peer", "expected_revision": 1, "intent": map[string]any{"target_user_id": memberSession.UserID, "action": "admit", "role": "member"}}, ownerSession.CSRF)
	if status != 200 {
		t.Fatalf("admit peer human: %d %s", status, body)
	}
	one := grantsPGEnroll(t, f, native, owner, ownerSession.CSRF, "sender")
	two := grantsPGEnroll(t, f, native, member, memberSession.CSRF, "receiver")
	sender := grantsPGBind(t, f, native, owner, ownerSession.CSRF, "room-two-endpoints", one, model.ActorSlot1, "sender")
	receiver := grantsPGBind(t, f, native, owner, ownerSession.CSRF, "room-two-endpoints", two, model.ActorSlot2, "receiver")
	endpoint := "/enterprise/device/v1/rooms/room-two-endpoints/relay"
	send := grantsPGRelayRequest{ClientID: "send-1", BindingID: sender.bindingID, Generation: sender.generation, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: model.ActorSlot2, Text: "complete addressed wire body", ExpectedPeerGeneration: receiver.generation}}
	status, body = grantsPGCall(t, f, native, http.MethodPost, endpoint, sender.token, send)
	if status != 200 {
		t.Fatalf("remote send: %d %s", status, body)
	}
	var sent grantsPGRelayReply
	if err := json.Unmarshal(body, &sent); err != nil || sent.Result.Message == nil {
		t.Fatal("send receipt missing canonical message")
	}
	claim := grantsPGRelayRequest{ClientID: "claim-1", BindingID: receiver.bindingID, Generation: receiver.generation, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}}
	status, body = grantsPGCall(t, f, native, http.MethodPost, endpoint, receiver.token, claim)
	if status != 200 {
		t.Fatalf("remote claim: %d %s", status, body)
	}
	var claimed grantsPGRelayReply
	if err := json.Unmarshal(body, &claimed); err != nil || claimed.Result.Claim == nil {
		t.Fatal("claim receipt missing private delivery")
	}
	delivery := claimed.Result.Claim
	if delivery.ID != sent.Result.Message.ID || delivery.Receipt == "" || !strings.Contains(delivery.Envelope, "complete addressed wire body") {
		t.Fatal("claim did not retain original complete body and private receipt")
	}
	status, history := f.get(owner, "/enterprise/api/v1/rooms/room-two-endpoints")
	if status != 200 || bytes.Contains(history, []byte(delivery.Receipt)) || bytes.Contains(history, []byte(`"receipt"`)) || bytes.Contains(history, []byte(`"events"`)) {
		t.Fatal("public Room projection leaked a delivery capability")
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, endpoint+"/resolve", receiver.token, claim)
	if status != 200 {
		t.Fatalf("claim resolution: %d %s", status, body)
	}
	var resolved grantsPGRelayReply
	if err := json.Unmarshal(body, &resolved); err != nil || resolved.Outcome != sqlstore.OutcomeReplayed || resolved.Result.Claim == nil || resolved.Result.Claim.Receipt != delivery.Receipt {
		t.Fatal("claim verification minted a different receipt")
	}
	ack := grantsPGRelayRequest{ClientID: "ack-1", BindingID: receiver.bindingID, Generation: receiver.generation, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: delivery.ID, Receipt: delivery.Receipt}}
	status, body = grantsPGCall(t, f, native, http.MethodPost, endpoint, receiver.token, ack)
	if status != 200 || bytes.Contains(body, []byte(delivery.Receipt)) {
		t.Fatalf("explicit receiver acknowledgement leaked a receipt: status=%d", status)
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, endpoint+"/resolve", sender.token, send)
	if status != 200 {
		t.Fatalf("send resolution: %d %s", status, body)
	}
	if err := json.Unmarshal(body, &resolved); err != nil || resolved.Result.Message == nil || resolved.Result.Message.ID != sent.Result.Message.ID {
		t.Fatal("send verification duplicated the message")
	}
	status, history = f.get(owner, "/enterprise/api/v1/rooms/room-two-endpoints")
	if status != 200 || bytes.Contains(history, []byte(delivery.Receipt)) {
		t.Fatal("acknowledged receipt became public")
	}
	var projection struct {
		Messages []relay.Message `json:"messages"`
	}
	if err := json.Unmarshal(history, &projection); err != nil || len(projection.Messages) != 1 || projection.Messages[0].State != "handed_off" {
		t.Fatal("public projection did not retain one explicitly acknowledged message")
	}
}

func TestGrantsHTTPPGFreshOIDCGovernanceKeepsOriginalSessionAndNeverGrantsAdminContent(t *testing.T) {
	f := newHTTPPGFixture(t)
	f.providerConfig.Reauthentication = enterpriseauth.ReauthenticatePromptAndMaxAge
	f.installHandler(f.store)
	owner, target, operator := f.browser("member-one"), f.browser("member-two"), f.browser("operator")
	f.login(owner)
	f.login(target)
	f.login(operator)
	ownerSession, targetSession, operatorSession := f.session(owner), f.session(target), f.session(operator)
	f.createRoom(owner, ownerSession.CSRF, "room-governance")
	status, body := f.post(owner, "/enterprise/api/v1/rooms/room-governance/memberships", map[string]any{"client_id": "admit-target", "expected_revision": 1, "intent": map[string]string{"target_user_id": targetSession.UserID, "action": "admit", "role": "member"}}, ownerSession.CSRF)
	if status != 200 {
		t.Fatalf("admit eligible target: %d %s", status, body)
	}
	status, body = f.get(owner, "/enterprise/api/v1/rooms/room-governance")
	if status != 200 {
		t.Fatal("owner read failed")
	}
	var view struct {
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	operatorCookie := f.cookie(operator)
	authenticated, err := f.store.AuthenticateSession(context.Background(), tokenHash(operatorCookie.Value))
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.store.SetUserEnabled(context.Background(), authenticated.Auth, "disable-owner-http", ownerSession.UserID, false)
	if err != nil || result.Outcome != sqlstore.OutcomeCommitted {
		t.Fatalf("explicit administrator disable: outcome=%s error=%v", result.Outcome, err)
	}
	status, _ = f.get(operator, "/enterprise/api/v1/rooms/room-governance")
	if status != 403 {
		t.Fatal("administrator inherited Room content access before governance")
	}
	intent := sqlstore.GovernanceIntent{RoomID: "room-governance", TargetUserID: targetSession.UserID, ExpectedOwnerUserID: ownerSession.UserID, ExpectedRevision: view.Revision, Reason: "Recover disabled Owner after verified operator request"}
	prepare := map[string]any{"client_id": "prepare-governance-http", "intent": intent}
	status, body = f.post(operator, "/enterprise/api/v1/governance/actions", prepare, operatorSession.CSRF)
	if status != 200 {
		t.Fatalf("prepare governance: %d %s", status, body)
	}
	var prepared struct {
		Action           sqlstore.GovernanceAction `json:"action"`
		AuthorizationURL string                    `json:"authorization_url"`
	}
	if err := json.Unmarshal(body, &prepared); err != nil || prepared.Action.ID == "" {
		t.Fatal("governance preparation missing action")
	}
	transfer := map[string]any{"client_id": "transfer-governance-http", "intent": intent, "action_id": prepared.Action.ID}
	status, _ = f.post(operator, "/enterprise/api/v1/rooms/room-governance/ownership", transfer, operatorSession.CSRF)
	if status < 400 {
		t.Fatal("an ordinary login session substituted for fresh action evidence")
	}
	authorization, err := url.Parse(prepared.AuthorizationURL)
	if err != nil || authorization.Query().Get("prompt") != "login" || authorization.Query().Get("max_age") != "0" {
		t.Fatal("step-up did not request configured fresh authentication")
	}
	// A freshly signed token for another IdP account must not bind proof to
	// the original administrator's live application session.
	idpURL, _ := url.Parse(f.idp.server.URL)
	operator.Jar.SetCookies(idpURL, []*http.Cookie{{Name: "fixture_subject", Value: "member-two", Path: "/", Secure: true, HttpOnly: true}})
	response, err := operator.Get(prepared.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode < 400 {
		t.Fatal("different upstream subject completed administrator step-up")
	}
	operator.Jar.SetCookies(idpURL, []*http.Cookie{{Name: "fixture_subject", Value: "operator", Path: "/", Secure: true, HttpOnly: true}})
	status, body = f.post(operator, "/enterprise/api/v1/governance/actions", prepare, operatorSession.CSRF)
	if status != 200 {
		t.Fatalf("retry explicit reauthentication: %d %s", status, body)
	}
	if err := json.Unmarshal(body, &prepared); err != nil {
		t.Fatal(err)
	}
	response, err = operator.Get(prepared.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("same original administrator step-up: %d", response.StatusCode)
	}
	if after := f.cookie(operator); after == nil || after.Value != operatorCookie.Value {
		t.Fatal("step-up minted or switched the application session")
	}
	status, body = f.get(operator, "/enterprise/api/v1/governance/actions/"+prepared.Action.ID)
	if status != 200 {
		t.Fatalf("read bound proof status: %d %s", status, body)
	}
	var ready struct {
		Action sqlstore.GovernanceAction `json:"action"`
	}
	if err := json.Unmarshal(body, &ready); err != nil || !ready.Action.Ready {
		t.Fatal("verified fresh token did not prepare action-bound evidence")
	}
	status, body = f.post(operator, "/enterprise/api/v1/rooms/room-governance/ownership", transfer, operatorSession.CSRF)
	if status != 200 {
		t.Fatalf("governance transfer: %d %s", status, body)
	}
	status, _ = f.get(operator, "/enterprise/api/v1/rooms/room-governance")
	if status != 403 {
		t.Fatal("governance operation granted administrator content access")
	}
	status, _ = f.get(target, "/enterprise/api/v1/rooms/room-governance")
	if status != 200 {
		t.Fatal("eligible new Owner could not read its Room")
	}
	var newOwner string
	if err := f.admin.QueryRow(context.Background(), `SELECT owner_user_id FROM rooms WHERE room_id='room-governance'`).Scan(&newOwner); err != nil || newOwner != targetSession.UserID {
		t.Fatal("governance did not commit exact intended Owner")
	}
	transfer["client_id"] = "reuse-consumed-proof"
	status, _ = f.post(operator, "/enterprise/api/v1/rooms/room-governance/ownership", transfer, operatorSession.CSRF)
	if status < 400 {
		t.Fatal("consumed action proof authorized another transfer")
	}
	status, _ = f.get(owner, "/enterprise/api/v1/session")
	if status != 401 {
		t.Fatal("disabled prior Owner's session was revived")
	}
}
