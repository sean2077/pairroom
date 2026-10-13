package relayclient

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseclient"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

// EnterpriseTransport is the original selected route and local record pointer.
// All bearer credentials remain in the owner-private enterprise client store.
type EnterpriseTransport struct {
	Origin       string `json:"origin"`
	DeploymentID string `json:"deployment_id"`
	UserID       string `json:"user_id"`
	DeviceID     string `json:"device_id"`
	AgentID      string `json:"agent_id"`
	RoomID       string `json:"room_id"`
	RecordID     string `json:"record_id"`
	Directory    string `json:"directory"`
	CAFile       string `json:"ca_file,omitempty"`
}

func enterpriseRoutingID(origin, deployment, room string) string {
	return "ent_" + relay.Digest(origin + "\x00" + deployment + "\x00" + room)[:32]
}
func validEnterpriseRoute(s State) bool {
	v := s.Enterprise
	if v == nil {
		return false
	}
	u, err := url.Parse(v.Origin)
	return s.Schema == 4 && s.LAN == nil && s.EndpointPath == "" && err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && v.DeploymentID != "" && v.UserID != "" && v.DeviceID != "" && v.AgentID != "" && v.RoomID != "" && safePart(v.RecordID) && filepath.IsAbs(v.Directory) && (v.CAFile == "" || filepath.IsAbs(v.CAFile)) && s.Room == enterpriseRoutingID(v.Origin, v.DeploymentID, v.RoomID)
}
func enterpriseConfig(v *EnterpriseTransport) enterpriseclient.Config {
	return enterpriseclient.Config{Origin: v.Origin, DeploymentID: v.DeploymentID, Directory: v.Directory, CAFile: v.CAFile}
}
func matchesEnterpriseMetadata(s State, m enterpriseclient.BindingMetadata) bool {
	return m.Status == "active" && matchesEnterpriseIdentity(s, m)
}
func matchesEnterpriseIdentity(s State, m enterpriseclient.BindingMetadata) bool {
	v := s.Enterprise
	return validEnterpriseRoute(s) && m.RecordID == v.RecordID && m.Origin == v.Origin && m.DeploymentID == v.DeploymentID && m.UserID == v.UserID && m.DeviceID == v.DeviceID && m.AgentID == v.AgentID && m.RoomID == v.RoomID && m.BindID == s.BindID && m.Generation == s.Generation && m.Slot == s.Slot && m.Runtime == s.Runtime && m.SessionHash == relay.Digest(string(s.Runtime)+"\x00"+s.SessionID) && sameWorkspace(m.Workspace, s.Workspace)
}

func currentEnterpriseBinding(s State) (bool, error) {
	if s.Enterprise == nil {
		return true, nil
	}
	if !validEnterpriseRoute(s) || privatefile.CheckDirectory(s.Enterprise.Directory) != nil {
		return false, enterpriseclient.ErrPrivateState
	}
	parent, err := enterpriseclient.New(enterpriseConfig(s.Enterprise))
	if err != nil {
		return false, err
	}
	defer parent.Close()
	client, err := parent.OpenBinding(s.Enterprise.RecordID)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	meta, err := client.Metadata(ctx)
	if errors.Is(err, enterpriseclient.ErrNoSession) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if meta.Status == "inactive" {
		return false, nil
	}
	if !matchesEnterpriseMetadata(s, meta) {
		return false, enterpriseclient.ErrPrivateState
	}
	return true, nil
}

func (c *Client) loadEnterprise() error {
	if !validEnterpriseRoute(c.State) || privatefile.CheckDirectory(c.State.Enterprise.Directory) != nil {
		return enterpriseclient.ErrPrivateState
	}
	parent, err := enterpriseclient.New(enterpriseConfig(c.State.Enterprise))
	if err != nil {
		return err
	}
	binding, err := parent.OpenBinding(c.State.Enterprise.RecordID)
	if err != nil {
		parent.Close()
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	meta, err := binding.Metadata(ctx)
	if err != nil {
		parent.Close()
		return err
	}
	if !matchesEnterpriseMetadata(c.State, meta) || meta.CredentialHash != relay.Digest(c.Secret) {
		parent.Close()
		return relay.ErrAuth
	}
	c.Enterprise, c.enterpriseParent = binding, parent
	return nil
}

func enterprisePrivateStatePath(path string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i := 1; i < len(parts); i++ {
		if parts[i-1] == ".pairroom" && parts[i] == "enterprise-joins" {
			return true
		}
		if i >= 2 && parts[i-2] == ".pairroom" && parts[i-1] == "rooms" && strings.HasPrefix(parts[i], "ent_") {
			return true
		}
	}
	return false
}

func enterprisePayload(c *Client, action string, payload any) (any, error) {
	if action != "publication" {
		return payload, nil
	}
	if c.State.Pending == nil {
		return nil, errors.New("enterprise publication recovery requires its original local WAL")
	}
	p := c.State.Pending
	return map[string]any{"report_seq": p.Seq, "text": p.Text, "expected_peer_generation": p.ExpectedPeerGeneration}, nil
}

func normalizeEnterpriseError(err error) error {
	var operation *enterpriseclient.Error
	if errors.As(err, &operation) {
		if operation.Class == relay.PeerChangedCode {
			return relay.ErrPeerChanged
		}
		if operation.Outcome == "unknown" {
			return relay.ErrUnknown
		}
		return &relayError{action: "enterprise", message: operation.Error(), code: operation.Class}
	}
	return err
}

func matchesEnterpriseSelection(s State, o options) bool {
	if o.enterpriseOrigin == "" {
		return true
	}
	v := s.Enterprise
	return v != nil && o.enterpriseOrigin == v.Origin && o.enterpriseDeployment == v.DeploymentID && (o.enterpriseCA == "" || sameWorkspace(o.enterpriseCA, v.CAFile)) && (o.enterpriseDirectory == "" || sameWorkspace(o.enterpriseDirectory, v.Directory))
}
func inheritEnterpriseSelection(s State, o *options) {
	if v := s.Enterprise; v != nil {
		o.enterpriseOrigin, o.enterpriseDeployment, o.enterpriseCA, o.enterpriseDirectory = v.Origin, v.DeploymentID, v.CAFile, v.Directory
	}
}
