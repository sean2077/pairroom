package relayclient

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

// statePaths visits only .pairroom/rooms/<room>/slots/<current-slot>/state.json.
// Logs, media, staging journals and retired slot directories are not discovery
// inputs. Prune before descending, but reject symlinks at every relevant level.
func statePaths(root string) ([]string, error) {
	base := filepath.Join(root, ".pairroom")
	info, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("relay workspace state must not be a symlink or non-directory")
	}
	var paths []string
	err = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		relevant := rel == "." || (parts[0] == "rooms" && len(parts) <= 5 &&
			(len(parts) < 2 || safePart(parts[1])) &&
			(len(parts) < 3 || parts[2] == "slots") &&
			(len(parts) < 4 || model.ActorID(parts[3]).ValidParticipant()) &&
			(len(parts) < 5 || parts[4] == "state.json"))
		if !relevant {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("relay state contains a symlink; refusing discovery")
		}
		if len(parts) == 5 {
			if !d.Type().IsRegular() {
				return errors.New("relay state must be a regular file")
			}
			paths = append(paths, path)
		} else if !d.IsDir() {
			return errors.New("relay state parent must be a directory")
		}
		return nil
	})
	return paths, err
}
