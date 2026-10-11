package service

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

func prepareLANSelections(owner model.ActorID, input map[model.ActorID]model.AgentSelection) (map[model.ActorID]model.AgentSelection, error) {
	if !owner.ValidParticipant() {
		return nil, errors.New("LAN creation requires owner_slot slot1 or slot2")
	}
	local, ok := input[owner]
	if !ok || local.AwaitingPeer || !local.Runtime.Valid() {
		return nil, errors.New("LAN creation requires the local owner's actual runtime")
	}
	peer := model.OtherParticipant(owner)
	if other, ok := input[peer]; ok && !other.AwaitingPeer {
		return nil, errors.New("LAN peer runtime is selected only when its owner-approved request is admitted")
	}
	if len(input) > 2 {
		return nil, errors.New("invalid LAN Agent selections")
	}
	return map[model.ActorID]model.AgentSelection{owner: local, peer: {AwaitingPeer: true}}, nil
}
func validateLANSelections(mode model.HostMode, sharing string, owner model.ActorID, agents map[model.ActorID]model.AgentSelection) error {
	if sharing == "" {
		if owner != "" {
			return errors.New("owner_slot requires LAN sharing")
		}
		for _, a := range agents {
			if a.AwaitingPeer {
				return errors.New("awaiting_peer requires a Native LAN Room")
			}
		}
		return nil
	}
	if sharing != "lan" || mode != model.HostNative || !owner.ValidParticipant() {
		return errors.New("LAN sharing requires Native mode and one local owner slot")
	}
	if len(agents) != 2 {
		return errors.New("LAN Room requires owner and awaiting peer selections")
	}
	if a := agents[owner]; a.AwaitingPeer || !a.Runtime.Valid() {
		return errors.New("LAN owner runtime must be selected")
	}
	if a := agents[model.OtherParticipant(owner)]; !a.AwaitingPeer && !a.Runtime.Valid() {
		return errors.New("LAN peer must await admission or name its admitted runtime")
	}
	return nil
}

type lanHostConfig struct {
	Schema   int               `json:"schema"`
	Enabled  bool              `json:"enabled"`
	Address  string            `json:"address,omitempty"`
	Identity lanshare.Identity `json:"identity"`
}
type lanHostStatus struct {
	Enabled    bool   `json:"enabled"`
	Address    string `json:"address"`
	Endpoint   string `json:"endpoint,omitempty"`
	HostPin    string `json:"host_pin,omitempty"`
	Diagnostic string `json:"diagnostic,omitempty"`
}
type lanRateWindow struct {
	At    time.Time
	Count int
}

// maxLANConcurrent bounds every in-flight authorized LAN request. The per-key
// and per-Room shares keep one member or one hosted Room from occupying the
// whole listener and starving the members of other Rooms.
const (
	maxLANConcurrent        = 64
	maxLANConcurrentPerKey  = 16
	maxLANConcurrentPerRoom = 24
)

type lanHostServer struct {
	mu            sync.Mutex
	owner         *ManagementServer
	path          string
	config        lanHostConfig
	http          *http.Server
	listener      net.Listener
	endpoint      string
	diagnostic    string
	rates         map[string]lanRateWindow
	inflight      map[string]int
	roomLoad      map[string]int
	inflightTotal int
	transfers     int
	roomTransfers map[string]int
}

func initLANHost(s *ManagementServer) error {
	dir, err := lanGuestPrivateDir(s.registry.Root(), "lan")
	if err != nil {
		return err
	}
	h := &lanHostServer{owner: s, path: filepath.Join(dir, "host.json"), config: lanHostConfig{Schema: 1}, rates: make(map[string]lanRateWindow), inflight: make(map[string]int)}
	info, err := os.Lstat(h.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		_ = info
		if err := privatefile.ReadJSON(h.path, 16<<10, &h.config); err != nil || h.config.Schema != 1 {
			return errors.New("invalid LAN host state; recover its original identity")
		}
		if h.config.Identity.CertificatePEM != "" {
			if _, err := h.config.Identity.Certificate(); err != nil {
				return errors.New("invalid LAN host identity")
			}
		}
		if h.config.Enabled {
			if h.config.Identity.CertificatePEM == "" {
				return errors.New("enabled LAN host lacks identity")
			}
			if err := h.startLocked(h.config); err != nil {
				h.diagnostic = "LAN listener unavailable; check the configured local interface and port"
			}
		}
	}
	s.lanHost = h
	return nil
}
func (h *lanHostServer) status() lanHostStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	pin, _ := h.config.Identity.Fingerprint()
	return lanHostStatus{Enabled: h.config.Enabled, Address: h.config.Address, Endpoint: h.endpoint, HostPin: pin, Diagnostic: h.diagnostic}
}
func (h *lanHostServer) startLocked(cfg lanHostConfig) error {
	if err := lanshare.ValidateEndpoint(lanshare.EndpointForAddress(cfg.Address)); err != nil {
		return err
	}
	config, err := lanshare.ServerTLS(cfg.Identity)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: http.HandlerFunc(h.serve), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 45 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
	h.http = srv
	h.listener = listener
	h.endpoint = lanshare.EndpointForAddress(listener.Addr().String())
	h.diagnostic = ""
	go func() {
		// A listener that dies after start must stop reporting itself as
		// available: its endpoint would otherwise still mint invitations for a
		// socket nobody serves.
		if err := srv.Serve(tls.NewListener(listener, config)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			h.mu.Lock()
			if h.http == srv {
				h.endpoint = ""
				h.diagnostic = "LAN listener stopped unexpectedly; save the LAN settings again to reopen it"
			}
			h.mu.Unlock()
		}
	}()
	return nil
}
func (h *lanHostServer) close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeLocked()
}
func (h *lanHostServer) closeLocked() {
	// Serve registers its listener asynchronously. Own the listening socket
	// here so close/reconfigure releases the port even before Serve starts.
	if h.listener != nil {
		_ = h.listener.Close()
		h.listener = nil
	}
	if h.http != nil {
		_ = h.http.Close()
		h.http = nil
	}
	h.endpoint = ""
}
func (h *lanHostServer) configure(enabled bool, address string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if enabled {
		if err := validateLANListenAddress(address); err != nil {
			return err
		}
	}
	if enabled && h.http != nil && h.config.Address == address {
		return nil
	}
	next := h.config
	next.Enabled = enabled
	next.Address = address
	if enabled && next.Identity.CertificatePEM == "" {
		identity, err := lanshare.NewIdentity()
		if err != nil {
			return err
		}
		next.Identity = identity
	}
	// Persist the identity and opt-in before opening a listening socket. A
	// restart keeps its key; a failed bind stays explicitly unavailable.
	if err := writeLANHostConfig(h.path, next); err != nil {
		return err
	}
	h.closeLocked()
	h.config = next
	h.diagnostic = ""
	if enabled {
		if err := h.startLocked(next); err != nil {
			h.diagnostic = "LAN listener unavailable; check the configured local interface and port"
			return err
		}
	}
	return nil
}
func writeLANHostConfig(path string, value lanHostConfig) error {
	return privatefile.WriteJSON(path, value)
}

func (s *ManagementServer) mountLANHost(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/lan", func(w http.ResponseWriter, _ *http.Request) { writeManagementJSON(w, 200, s.lanHost.status()) })
	mux.HandleFunc("PUT /api/v1/lan", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled bool   `json:"enabled"`
			Address string `json:"address"`
		}
		if decodeManagementJSON(w, r, &req) != nil {
			return
		}
		if err := s.lanHost.configure(req.Enabled, req.Address); err != nil {
			writeManagementError(w, 400, err.Error())
			return
		}
		writeManagementJSON(w, 200, s.lanHost.status())
	})
	mux.HandleFunc("GET /api/v1/rooms/{room}/lan", s.readLANRoom)
	mux.HandleFunc("POST /api/v1/rooms/{room}/lan/{action}", s.manageLANRoom)
}
func (s *ManagementServer) sharedNativeRuntime(ctx context.Context, id string) (*nativeHostRuntime, error) {
	room, ok := s.registry.Room(id)
	if !ok || room.Sharing != "lan" || room.HostMode != model.HostNative || room.Archived() {
		return nil, ErrRoomNotFound
	}
	return s.nativeRuntime(ctx, id)
}
func (s *ManagementServer) readLANRoom(w http.ResponseWriter, r *http.Request) {
	n, err := s.sharedNativeRuntime(r.Context(), r.PathValue("room"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	release := n.acquire()
	defer release()
	writeManagementJSON(w, 200, n.engine.LANState())
}
func (s *ManagementServer) manageLANRoom(w http.ResponseWriter, r *http.Request) {
	unlock := s.lockRoom(r.PathValue("room"))
	defer unlock()
	n, err := s.sharedNativeRuntime(r.Context(), r.PathValue("room"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	release := n.acquire()
	defer release()
	s.lanOwnerAction(w, r, n, r.PathValue("action"))
}
func (s *ManagementServer) lanOwnerAction(w http.ResponseWriter, r *http.Request, n *nativeHostRuntime, action string, owner ...relay.Auth) {
	var req struct {
		Receipt string `json:"receipt,omitempty"`
	}
	if decodeNativeJSON(w, r, &req) != nil {
		return
	}
	switch action {
	case "invite":
		status := s.lanHost.status()
		if status.Endpoint == "" {
			writeManagementError(w, 409, "Enable LAN sharing in Service Settings and select a numeric local interface first")
			return
		}
		v, err := n.engine.CreateLANInvite(owner...)
		if err != nil {
			nativeResult(w, nil, err)
			return
		}
		invite := lanshare.Invite{Version: lanshare.Version, Endpoint: status.Endpoint, HostPin: status.HostPin, RoomID: n.room.ID, InviteID: v.ID, ExpiresAt: v.ExpiresAt}
		writeManagementJSON(w, 200, map[string]any{"invite": lanshare.EncodeInvite(invite), "expires_at": v.ExpiresAt})
	case "accept":
		receipt, err := lanshare.ParseReceipt(req.Receipt)
		if err != nil || receipt.RoomID != n.room.ID {
			writeManagementError(w, 400, "receipt must identify this Room and the exact peer public key")
			return
		}
		b, err := n.engine.AcceptLANJoin(receipt.RequestID, receipt.Fingerprint, owner...)
		if err != nil {
			// A stale or unrecognized receipt is an input the operator can fix;
			// reporting it as 401 would end the browser session.
			if errors.Is(err, relay.ErrAuth) {
				writeManagementJSON(w, 400, map[string]string{"error": "no live join request matches this exact receipt; request a fresh invitation and retry with the receipt for that request", "code": "lan_receipt_stale"})
				return
			}
			nativeResult(w, nil, err)
			return
		}
		writeManagementJSON(w, 200, lanshare.JoinResponse{Status: "accepted", Receipt: req.Receipt, Room: s.lanRoomInfo(n, b)})
	case "revoke":
		nativeResult(w, map[string]bool{"revoked": true}, n.engine.RevokeLANMember(owner...))
	default:
		writeManagementError(w, 404, "unknown LAN owner operation")
	}
}
func (s *ManagementServer) lanRoomInfo(n *nativeHostRuntime, b relay.Binding) *lanshare.RoomInfo {
	name := n.room.Name
	if room, ok := s.registry.Room(n.room.ID); ok {
		name = room.Name
	}
	return &lanshare.RoomInfo{RoomID: n.room.ID, Name: name, Slot: b.Slot, Generation: b.Generation, BindID: b.BindID, Runtimes: n.engine.Runtimes(), Collaboration: model.CloneCollaboration(n.room.Collaboration)}
}

// enter takes one in-flight capacity slot for a certified key of one Room. It
// refuses when the listener, that key or that Room already holds its share.
func (h *lanHostServer) enter(room, key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflightTotal >= maxLANConcurrent || h.inflight[key] >= maxLANConcurrentPerKey || h.roomLoad[room] >= maxLANConcurrentPerRoom {
		return false
	}
	if h.inflight == nil {
		h.inflight = make(map[string]int)
	}
	if h.roomLoad == nil {
		h.roomLoad = make(map[string]int)
	}
	h.inflight[key]++
	h.roomLoad[room]++
	h.inflightTotal++
	return true
}

// leave releases the slot held by enter, keeping both maps bounded by the
// in-flight requests themselves.
func (h *lanHostServer) leave(room, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if count := h.inflight[key]; count > 1 {
		h.inflight[key] = count - 1
	} else {
		delete(h.inflight, key)
	}
	if count := h.roomLoad[room]; count > 1 {
		h.roomLoad[room] = count - 1
	} else {
		delete(h.roomLoad, room)
	}
	h.inflightTotal--
}

func (h *lanHostServer) admitRequest(r *http.Request) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.config.Enabled {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	host = ip.Unmap().String()
	now := time.Now()
	v, known := h.rates[host]
	if !known && len(h.rates) >= 1024 {
		for k, v := range h.rates {
			if now.Sub(v.At) >= time.Minute {
				delete(h.rates, k)
			}
		}
		// Table pressure rejects only untracked sources. An existing source
		// retains its own rate budget; a burst of other addresses cannot turn
		// this bounded table into a global one-minute denial of service.
		if len(h.rates) >= 1024 {
			return false
		}
	}
	if now.Sub(v.At) >= time.Minute {
		v = lanRateWindow{At: now}
	}
	v.Count++
	if h.rates == nil {
		h.rates = make(map[string]lanRateWindow)
	}
	h.rates[host] = v
	return v.Count <= 240
}
func (h *lanHostServer) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Origin") != "" || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
		writeManagementError(w, 403, "LAN client certificate request required")
		return
	}
	key := lanshare.Fingerprint(r.TLS.PeerCertificates[0])
	// Throttle by source before any other work, then resolve the route: neither
	// an unauthenticated flood nor a bogus route may reach the capacity gate.
	if !h.admitRequest(r) {
		writeManagementError(w, 429, "LAN request limit reached")
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/lan/v1/rooms/")
	room, action, found := strings.Cut(path, "/")
	if !ok || !found || !lanshare.ValidID(room) || !lanshare.ValidID(action) || !allowedLANAction(action) {
		writeManagementError(w, 404, "unknown LAN route")
		return
	}
	n, err := h.authorizedRuntime(r, room, action, key)
	if err != nil {
		if errors.Is(err, relay.ErrAuth) {
			writeManagementError(w, 403, "shared Room unavailable or membership does not authorize this operation")
			return
		}
		if errors.Is(err, errLANHostSuspended) {
			writeManagementJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the hosting Room is suspended; a member command such as relay status activates it — retry doctor afterwards", "code": lanshare.HostUnavailableCode})
			return
		}
		// The certificate is admitted but the hosting Service cannot serve the
		// Room right now (a Runtime that failed to start, a suspended Room, an
		// unhealthy Registry). Reporting that as an authorization failure would
		// tell a correctly admitted member its admission was revoked.
		writeManagementJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the hosting Room is unavailable on this Service right now; the membership stays valid — retry after the host resolves it", "code": lanshare.HostUnavailableCode})
		return
	}
	// Only a provably authorized member of this exact Room holds a capacity
	// slot: client certificates are self-minted, so an unauthenticated device
	// must never be able to occupy the listener or starve other Rooms.
	if !h.enter(room, key) {
		writeManagementError(w, 429, "LAN request capacity reached")
		return
	}
	defer h.leave(room, key)
	release := n.acquire()
	defer release()

	if action == "join" || action == "join-status" {
		h.serveJoin(w, r, n, key, action)
		return
	}
	auth, err := n.engine.LANAuth(key)
	if err != nil {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	generation, err := strconv.ParseUint(r.Header.Get("X-PairRoom-LAN-Generation"), 10, 64)
	if err != nil || generation != auth.Generation || r.Header.Get("X-PairRoom-LAN-Bind") != auth.BindID {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	h.serveMember(w, r, n, auth, action)
}
func (h *lanHostServer) serveJoin(w http.ResponseWriter, r *http.Request, n *nativeHostRuntime, key, action string) {
	var requestID, status string
	var err error
	if action == "join" {
		var req lanshare.JoinRequest
		if decodeManagementJSONLimit(w, r, &req, 4096) != nil {
			return
		}
		var j relay.LANJoinRequest
		j, status, err = n.engine.RequestLANJoin(relay.LANJoinRequest{InviteID: req.InviteID, RequestID: req.RequestID, Key: key, Runtime: req.Runtime, Label: req.Label})
		requestID = j.RequestID
	} else {
		var req lanshare.JoinStatusRequest
		if decodeManagementJSONLimit(w, r, &req, 4096) != nil {
			return
		}
		requestID = req.RequestID
		_, status, err = n.engine.LANJoinStatus(req.RequestID, key)
	}
	if err != nil {
		nativeResult(w, nil, err)
		return
	}
	resp := lanshare.JoinResponse{Status: status, Receipt: lanshare.EncodeReceipt(lanshare.Receipt{RoomID: n.room.ID, RequestID: requestID, Fingerprint: key})}
	if status == "accepted" {
		a, err := n.engine.LANAuth(key)
		if err != nil {
			nativeResult(w, nil, err)
			return
		}
		b, err := n.engine.Inspect(a)
		if err != nil {
			nativeResult(w, nil, err)
			return
		}
		resp.Room = h.owner.lanRoomInfo(n, b)
	}
	writeManagementJSON(w, 200, resp)
}
