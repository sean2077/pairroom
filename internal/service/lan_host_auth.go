package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func validateLANListenAddress(address string) error {
	if err := lanshare.ValidateEndpoint(lanshare.EndpointForAddress(address)); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(address)
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errors.New("numeric local LAN interface required")
	}
	// Loopback is intrinsically local; restricted/container environments may
	// deny interface enumeration even though their numeric loopback works.
	// Real LAN addresses still require an assigned-interface observation.
	if ip.IsLoopback() {
		return nil
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return errors.New("cannot inspect local network interfaces")
	}
	for _, addr := range addrs {
		prefix, err := netip.ParsePrefix(addr.String())
		if err == nil && prefix.Addr().Unmap() == ip.WithZone("").Unmap() {
			return nil
		}
	}
	return errors.New("select an address assigned to this machine's local interface")
}
func allowedLANAction(action string) bool {
	switch action {
	case "join", "join-status", "inspect", "confirm", "room", "report", "publication", "send", "user-send", "user-receipt", "head", "claim", "ack", "status", "summary", "history", "doctor", "peer", "failure", "park", "leave", "unbind", "attachment", "download", "upload", "wake-candidate", "wake-reserve", "wake-record":
		return true
	default:
		return false
	}
}

// The Registry builds this compact projection from validated Room facts on
// startup and updates it after each durable LAN append. It is only an early
// admission filter: every effect reauthenticates in the live Engine.
type lanRoomAuthorization struct {
	invites  map[string]relay.LANInvite
	requests map[string]relay.LANJoinRequest
	member   *relay.LANMember
	invalid  bool
}

func newLANRoomAuthorization() *lanRoomAuthorization {
	return &lanRoomAuthorization{invites: make(map[string]relay.LANInvite), requests: make(map[string]relay.LANJoinRequest)}
}

func (p *lanRoomAuthorization) apply(event model.Event) error {
	switch event.Kind {
	case relay.EventLANInvite:
		var v relay.LANInvite
		if json.Unmarshal(event.Data, &v) != nil || !lanshare.ValidID(v.ID) || v.ExpiresAt.IsZero() {
			return errors.New("invalid LAN invitation authorization fact")
		}
		// The writer issues a new invitation only after prior invitations have
		// expired. Old requests retain their exact invitation ID separately.
		for id, prior := range p.invites {
			if prior.Consumed || !event.CreatedAt.Before(prior.ExpiresAt) {
				delete(p.invites, id)
			}
		}
		p.invites[v.ID] = v
	case relay.EventLANJoin:
		var j relay.LANJoinRequest
		if json.Unmarshal(event.Data, &j) != nil || !lanshare.ValidID(j.RequestID) || !lanshare.ValidID(j.InviteID) || !lanshare.ValidFingerprint(j.Key) || !j.Runtime.Valid() {
			return errors.New("invalid LAN join authorization fact")
		}
		p.requests[j.RequestID] = j
	case relay.EventLANMember:
		var member relay.LANMember
		if json.Unmarshal(event.Data, &member) != nil {
			return errors.New("invalid LAN member authorization fact")
		}
		if _, err := relay.LANBindingFromEvent(event); err != nil {
			return err
		}
		p.member = &member
		if invite, ok := p.invites[member.InviteID]; ok {
			invite.Consumed = true
			p.invites[member.InviteID] = invite
		}
	}
	return nil
}

func (r *Registry) resetLANAuthorization(room Room, events []model.Event) error {
	if room.Sharing != "lan" {
		return nil
	}
	projection := newLANRoomAuthorization()
	for _, event := range events {
		if err := projection.apply(event); err != nil {
			return err
		}
	}
	r.lanAuthMu.Lock()
	defer r.lanAuthMu.Unlock()
	if r.lanAuth == nil {
		r.lanAuth = make(map[string]*lanRoomAuthorization)
	}
	r.lanAuth[room.ID] = projection
	return nil
}

func (r *Registry) observeLANAuthorization(event model.Event) {
	if event.Kind != relay.EventLANInvite && event.Kind != relay.EventLANJoin && event.Kind != relay.EventLANMember {
		return
	}
	r.lanAuthMu.Lock()
	defer r.lanAuthMu.Unlock()
	if r.lanAuth == nil {
		r.lanAuth = make(map[string]*lanRoomAuthorization)
	}
	projection := r.lanAuth[event.RoomID]
	if projection == nil {
		projection = newLANRoomAuthorization()
		r.lanAuth[event.RoomID] = projection
	}
	if err := projection.apply(event); err != nil {
		projection.invalid = true
	}
}

// A pending or unknown certificate cannot consume Runtime capacity or force a
// full history replay just by naming a suspended Room. No request path below
// opens an Event Log; only a proven invitation or membership can activate it.
func (h *lanHostServer) authorizedRuntime(r *http.Request, roomID, action, key string) (*nativeHostRuntime, error) {
	room, ok := h.owner.registry.Room(roomID)
	if !ok || room.HostMode != model.HostNative || room.Sharing != "lan" || room.Archived() {
		return nil, relay.ErrAuth
	}
	active, err := h.owner.runtimes.runtimeForCompletion(roomID)
	if err == nil {
		n, ok := active.(*nativeHostRuntime)
		if !ok {
			return nil, relay.ErrAuth
		}
		return n, nil
	}
	if action == "doctor" || !errors.Is(err, ErrRuntimeNotReady) {
		return nil, relay.ErrAuth
	}
	var join lanshare.JoinRequest
	var status lanshare.JoinStatusRequest
	if action == "join" || action == "join-status" {
		data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		_ = r.Body.Close()
		if err != nil || len(data) > 4096 {
			return nil, relay.ErrAuth
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		if action == "join" && json.Unmarshal(data, &join) != nil || action == "join-status" && json.Unmarshal(data, &status) != nil {
			return nil, relay.ErrAuth
		}
	}
	h.owner.registry.lanAuthMu.RLock()
	projection := h.owner.registry.lanAuth[roomID]
	allowed := false
	if projection != nil && !projection.invalid {
		switch action {
		case "join":
			v, found := projection.invites[join.InviteID]
			allowed = found && !v.Consumed && time.Now().Before(v.ExpiresAt) && lanshare.ValidID(join.RequestID) && join.Runtime.Valid()
			if old, found := projection.requests[join.RequestID]; found {
				allowed = old.Key == key && old.InviteID == join.InviteID && old.Runtime == join.Runtime
			}
		case "join-status":
			old, found := projection.requests[status.RequestID]
			allowed = found && old.Key == key
		default:
			if member := projection.member; member != nil {
				generation, err := strconv.ParseUint(r.Header.Get("X-PairRoom-LAN-Generation"), 10, 64)
				binding := room.Bindings[model.OtherParticipant(room.OwnerSlot)]
				allowed = err == nil && !binding.Pending && binding.RemoteKey == key && member.Binding.Active && member.Binding.RemoteKey == key && member.Binding.BindID == r.Header.Get("X-PairRoom-LAN-Bind") && member.Binding.Generation == generation
			}
		}
	}
	h.owner.registry.lanAuthMu.RUnlock()
	if !allowed {
		return nil, relay.ErrAuth
	}
	return h.owner.nativeRuntime(r.Context(), roomID)
}
