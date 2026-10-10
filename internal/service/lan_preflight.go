package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/privatefile"
)

// LAN identities are not derived data. Refuse unknown/corrupt identity formats
// before Registry cleanup or checkpoint upgrade, without generating a new key.
func preflightLANState(root string) error {
	clients, err := lanclient.Open()
	if err != nil {
		return errors.New("invalid LAN client store; data was not modified")
	}
	defer clients.Close()
	if _, err := clients.List(context.Background()); err != nil {
		return errors.New("unsupported or invalid LAN client identity; data was not modified")
	}
	dir := filepath.Join(root, "lan")
	if err := privatefile.CheckDirectory(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("invalid LAN identity directory; data was not modified")
	}
	var host lanHostConfig
	err = privatefile.ReadJSON(filepath.Join(dir, "host.json"), 16<<10, &host)
	if err == nil {
		if host.Schema != 1 || (host.Enabled && (host.Identity.CertificatePEM == "" || lanshare.ValidateEndpoint(lanshare.EndpointForAddress(host.Address)) != nil)) {
			return errors.New("unsupported or invalid LAN host identity; data was not modified")
		}
		if host.Identity.CertificatePEM != "" || host.Identity.PrivateKeyPEM != "" {
			if _, err := host.Identity.Certificate(); err != nil {
				return errors.New("invalid LAN host key pair; data was not modified")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot validate LAN host identity; data was not modified")
	}
	return nil
}
