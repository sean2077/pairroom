package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
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

// A pending/unknown certificate must not consume Runtime capacity just by
// naming a Room. Suspended Room admission reads public authorization facts
// without opening a writer; every effect reauthenticates in the live Engine.
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
	events, err := readEventsReadOnly(filepath.Join(room.DataDir, "events.jsonl"))
	if err != nil {
		return nil, relay.ErrAuth
	}
	invites := map[string]relay.LANInvite{}
	requests := map[string]relay.LANJoinRequest{}
	var member *relay.LANMember
	for _, event := range events {
		switch event.Kind {
		case relay.EventLANInvite:
			var v relay.LANInvite
			if json.Unmarshal(event.Data, &v) != nil {
				return nil, relay.ErrAuth
			}
			invites[v.ID] = v
		case relay.EventLANJoin:
			var j relay.LANJoinRequest
			if json.Unmarshal(event.Data, &j) != nil {
				return nil, relay.ErrAuth
			}
			requests[j.RequestID] = j
		case relay.EventLANMember:
			var m relay.LANMember
			if json.Unmarshal(event.Data, &m) != nil {
				return nil, relay.ErrAuth
			}
			member = &m
			v := invites[m.InviteID]
			v.Consumed = true
			invites[m.InviteID] = v
		}
	}
	allowed := false
	if action == "join" || action == "join-status" {
		data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		_ = r.Body.Close()
		if err != nil || len(data) > 4096 {
			return nil, relay.ErrAuth
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		if action == "join" {
			var j lanshare.JoinRequest
			if json.Unmarshal(data, &j) != nil {
				return nil, relay.ErrAuth
			}
			v, found := invites[j.InviteID]
			allowed = found && !v.Consumed && time.Now().Before(v.ExpiresAt)
			if old, found := requests[j.RequestID]; found {
				allowed = old.Key == key && old.InviteID == j.InviteID && old.Runtime == j.Runtime
			}
		} else {
			var j lanshare.JoinStatusRequest
			if json.Unmarshal(data, &j) != nil {
				return nil, relay.ErrAuth
			}
			old, found := requests[j.RequestID]
			allowed = found && old.Key == key
		}
	} else if member != nil {
		generation, err := strconv.ParseUint(r.Header.Get("X-PairRoom-LAN-Generation"), 10, 64)
		allowed = err == nil && member.Binding.Active && member.Binding.RemoteKey == key && member.Binding.BindID == r.Header.Get("X-PairRoom-LAN-Bind") && member.Binding.Generation == generation
	}
	if !allowed {
		return nil, relay.ErrAuth
	}
	return h.owner.nativeRuntime(r.Context(), roomID)
}
