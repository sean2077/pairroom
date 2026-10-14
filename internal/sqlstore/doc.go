// Package sqlstore is the enterprise transactional kernel for one deployment's
// Rooms and identity control. It provides one SQL transaction boundary: a Room
// fact or closed deployment-scoped control receipt, authoritative changes,
// audit and immutable operation identity commit under the same ownership fence
// and live authorization checks. Room writes also check the expected revision.
//
// The supported typed human operations are Room creation, text-only Human
// Send and owner-authorized membership admission, role change and removal,
// alongside the internal append_fact primitive. RoomPort connects the shared
// Relay projection without exposing a public service surface. Membership
// removal and viewer admission/demotion currently refuse an active target
// Binding in that Room rather than performing partial revocation; grant and
// Binding retirement commands are separate work.
// Explicit operator identity initialization, admission, administrative status
// and persistent browser sessions use the same runner; public Room commands
// recheck their session inside the guarded transaction, not only in middleware.
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
