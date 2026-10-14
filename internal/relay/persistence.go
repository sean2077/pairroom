package relay

import (
	"reflect"

	"github.com/sean2077/pairroom/internal/model"
)

// Persistence is the durable boundary one Room uses. The local JSONL store is
// the current implementation; a transactional enterprise backend must provide
// the same contract, so the engine never depends on a concrete store:
//
//   - Facts are append-only and Load returns them in Room sequence order.
//   - A nil Append return means the fact is durably readable afterwards.
//   - A non-nil Append return does not establish that no effect exists: the
//     write may have partially or fully persisted (the local store closes its
//     writer on a write or sync failure, and an uncertain tail may remain).
//     Callers fail closed on any append error: never publish success, never
//     treat the error as proof of absence, never retry an external effect on
//     a guess.
//
// The boundary deliberately stays this small, and the engine's handling of an
// uncertain append stays deliberately conservative. Adjudicating which fact an
// uncertain append committed — by that operation's immutable identity, payload
// and expected revision, serialized under a consistency guard — belongs to
// the transactional backend: a plain read that does not observe the fact
// cannot disprove a commit that is still in flight.
type Persistence interface {
	Append(event *model.Event) error
	Load() ([]model.Event, error)
	Close() error
}

// nilPersistence reports whether p is nil, including a typed-nil pointer held
// in the interface. Open refuses both, preserving the validation the former
// concrete *store.JSONLStore configuration field performed.
func nilPersistence(p Persistence) bool {
	if p == nil {
		return true
	}
	value := reflect.ValueOf(p)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	}
	return false
}
