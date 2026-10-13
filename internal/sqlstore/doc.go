// Package sqlstore is the enterprise transactional kernel for one deployment's
// Rooms. It provides the single-writer SQL transaction boundary the enterprise
// storage seam is built on: one Room fact, its audit record and its immutable
// operation identity commit atomically under the ownership fence, a strict
// expected-revision check and a live authorization check.
//
// The supported typed human operations are Room creation, text-only Human
// Send and owner-authorized membership admission, role change and removal,
// alongside the internal append_fact primitive. RoomPort connects the shared
// Relay projection without exposing a public service surface. Membership
// removal currently refuses an active target Binding rather than performing
// partial revocation; grant and Binding retirement commands are separate work.
// The design contract lives in docs/design/enterprise-service.md §13.
//
// Transaction discipline: the caller's context bounds every statement and the
// COMMIT (and an internal budget bounds every operation even with an
// unbounded caller context); an uncertain commit outcome is reported as
// OutcomeUnknown and is never guessed, retried or reconciled here — Resolve
// can verify the final state under the same fence and authorization, and the
// caller decides.
//
// This kernel is an internal API for trusted producers (the Room core). It
// grants no end-user, HTTP or raw-event upload surface. Only append_fact treats
// fact kinds as opaque; typed operations validate their closed codecs. Control
// rows, rather than replayed historical events, remain the authority for
// current authorization.
package sqlstore
