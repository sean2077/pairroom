// Package atomicfile replaces and reads small private state files that other
// local processes (hooks, CLI commands, the Service) may read concurrently.
// On POSIX it is plain rename/open. On Windows, readers share delete access and
// replacement uses POSIX-semantics rename, so a PairRoom reader never blocks a
// writer; a foreign reader without delete sharing (an older CLI, antivirus or an
// indexer) is waited out for a short bounded window instead of failing the save.
package atomicfile

import "io"

// ReadFile reads path through Open, with os.ReadFile's result semantics.
func ReadFile(path string) ([]byte, error) {
	f, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
