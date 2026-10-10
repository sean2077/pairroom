package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/privatefile"
)

// LAN identities are not derived data. Refuse unknown/corrupt identity formats
// before Registry cleanup or checkpoint upgrade, without generating a new key.
func preflightLANState(root string) error {
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
	guests := filepath.Join(dir, "guests")
	if err := privatefile.CheckDirectory(guests); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("invalid LAN guest identity directory; data was not modified")
	}
	entries, err := os.ReadDir(guests)
	if err != nil {
		return errors.New("cannot inspect LAN guest identities; data was not modified")
	}
	if len(entries) > maxLANGuests {
		return errors.New("LAN guest identity limit exceeded; data was not modified")
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue // a private atomic temporary is never a replacement identity
		}
		path := filepath.Join(guests, entry.Name())
		if !lanshare.ValidID(entry.Name()) || privatefile.CheckDirectory(path) != nil {
			return errors.New("invalid LAN guest identity directory; data was not modified")
		}
		var record lanGuestRecord
		err := privatefile.ReadJSON(filepath.Join(path, "guest.json"), 2<<20, &record)
		if errors.Is(err, os.ErrNotExist) {
			continue // mkdir before the first identity commit has no public effect
		}
		if err != nil || record.ID != entry.Name() || validateLANGuestRecord(record) != nil {
			return errors.New("unsupported or invalid LAN guest identity; data was not modified")
		}
	}
	return nil
}
