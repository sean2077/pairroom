package lanclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/relay"
)

// A request uses its immutable operation snapshot, not whichever binding a
// concurrent process most recently loaded. Native credentials never leave
// this machine: only the accepted LAN binding and generation are headers.
type memberTransport struct {
	base       http.RoundTripper
	bindID     string
	generation uint64
}

func (t memberTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	if t.bindID != "" {
		lanshare.SetMemberHeaders(copy, t.bindID, t.generation)
	}
	return t.base.RoundTrip(copy)
}

func (c *Client) httpFor(r record) (*http.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := relay.Digest(r.Invite.Endpoint + "\x00" + r.Invite.HostPin + "\x00" + r.Identity.CertificatePEM + "\x00" + r.Identity.PrivateKeyPEM)
	if c.http == nil || c.transportKey != key {
		client, err := lanshare.NewClient(r.Invite, r.Identity)
		if err != nil {
			return nil, err
		}
		if c.http != nil {
			c.http.CloseIdleConnections()
		}
		c.http, c.transportKey = client, key
	}
	client := *c.http
	transport := memberTransport{base: client.Transport}
	if r.Room != nil {
		transport.bindID, transport.generation = r.Room.BindID, r.Room.Generation
	}
	client.Transport = transport
	return &client, nil
}

func (c *Client) observe(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = err == nil
	if err == nil {
		c.lastSeen = time.Now().UTC()
	}
}

func (c *Client) call(ctx context.Context, r record, action string, payload, result any) error {
	client, err := c.httpFor(r)
	if err != nil {
		return err
	}
	err = lanshare.Call(ctx, client, r.Invite, action, payload, result)
	c.observe(err)
	return err
}

func membershipDenied(err error) bool {
	var failure *lanshare.Error
	return errors.As(err, &failure) && (failure.Status == http.StatusUnauthorized || failure.Status == http.StatusForbidden)
}

func safeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrTransportUnavailable) {
		return ErrTransportUnavailable
	}
	var localFailure *Error
	if errors.As(err, &localFailure) {
		return localFailure
	}
	if errors.Is(err, relay.ErrAuth) || errors.Is(err, nativeidentity.ErrOwned) || errors.Is(err, nativeidentity.ErrUnowned) || membershipDenied(err) {
		return relay.ErrAuth
	}
	if errors.Is(err, relay.ErrSendPayloadConflict) {
		return relay.ErrSendPayloadConflict
	}
	var failure *lanshare.Error
	if errors.As(err, &failure) {
		if failure.Code == relay.SendPayloadConflictCode {
			return relay.ErrSendPayloadConflict
		}
		if failure.Code == "wake_reserved" {
			return relay.ErrWakeReserved
		}
		return &Error{Status: failure.Status, Message: ErrUnavailable.Error()}
	}
	var transportFailure *url.Error
	if errors.As(err, &transportFailure) {
		return ErrTransportUnavailable
	}
	return ErrUnavailable
}
