package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/privatefile"
)

// LAN identities are not derived data. Refuse unknown/corrupt identity formats
// before Registry cleanup or checkpoint upgrade, without generating a new key.
//
// The machine-wide per-user joined-Room catalog is checked for the same reason:
// a catalog this build must not interpret is never repaired or regenerated, and
// the operator is told where it is and what to do instead of being left to
// guess. An owner-only boundary failure is different — the data is intact and
// the surfaces that use it already fail closed — so it is reported at runtime
// rather than blocking the Service.
func preflightLANState(root string) error {
	clients, openErr := lanclient.Open()
	if openErr == nil {
		defer clients.Close()
		if err := clients.PreflightCatalog(context.Background()); err != nil && !errors.Is(err, privatefile.ErrPrivate) {
			return fmt.Errorf("unsupported or invalid LAN client identity under %s; data was not modified: back it up, then repair or remove that per-user joined-Room catalog by hand (PairRoom never regenerates it automatically)", clients.Root())
		}
	} else {
		store, rootErr := lanclient.DefaultRoot()
		if rootErr != nil {
			return fmt.Errorf("invalid LAN client store; data was not modified: %v", openErr)
		}
		if !errors.Is(openErr, privatefile.ErrPrivate) {
			return fmt.Errorf("unsupported or invalid LAN client store under %s; data was not modified: back it up, then repair or remove that per-user joined-Room catalog by hand (PairRoom never regenerates it automatically)", store)
		}
	}
	dir := filepath.Join(root, "lan")
	if err := privatefile.CheckDirectory(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("invalid LAN identity directory; data was not modified")
	}
	var host lanHostConfig
	err := privatefile.ReadJSON(filepath.Join(dir, "host.json"), 16<<10, &host)
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
