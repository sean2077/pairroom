package lanclient

import (
	"context"
	"errors"
	"os"

	"github.com/sean2077/pairroom/internal/privatefile"
)

// PreflightCatalog checks every safely readable identity before Service
// recovery. A damaged owner-only boundary does not hide a later future or
// invalid record. It never repairs permissions, follows redirected entries,
// creates an identity, or changes a record.
func (s *Store) PreflightCatalog(ctx context.Context) error {
	if err := privatefile.CheckDirectory(s.root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	entries, err := readClientDirectories(s.root)
	if err != nil {
		return err
	}
	var boundaryErr error
	for _, entry := range entries {
		client, err := s.client(entry.Name())
		if err != nil {
			return err
		}
		_, err = client.read(ctx)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// An interrupted first mkdir contains no identity to interpret.
			continue
		case errors.Is(err, privatefile.ErrPrivate):
			boundaryErr = err
		case err != nil:
			return err
		}
	}
	return boundaryErr
}
