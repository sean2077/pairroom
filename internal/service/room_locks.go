package service

import "sync"

type roomLockEntry struct {
	mu    sync.Mutex
	users int
}

// roomLockSet provides per-Room serialization without retaining one mutex for
// every Room ever seen. users is incremented before waiting, so an entry cannot
// be removed while another caller is queued on its mutex.
type roomLockSet struct {
	mu      sync.Mutex
	entries map[string]*roomLockEntry
}

func (s *roomLockSet) Lock(roomID string) func() {
	s.mu.Lock()
	if s.entries == nil {
		s.entries = make(map[string]*roomLockEntry)
	}
	entry := s.entries[roomID]
	if entry == nil {
		entry = &roomLockEntry{}
		s.entries[roomID] = entry
	}
	entry.users++
	s.mu.Unlock()

	entry.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Unlock()
			s.mu.Lock()
			entry.users--
			if entry.users == 0 && s.entries[roomID] == entry {
				delete(s.entries, roomID)
			}
			s.mu.Unlock()
		})
	}
}
