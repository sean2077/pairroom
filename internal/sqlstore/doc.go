// Package sqlstore is the enterprise transactional kernel for one deployment's
// Rooms. It provides the single-writer SQL transaction boundary the enterprise
// storage seam is built on: one Room fact, its audit record and its immutable
// operation identity commit atomically under the ownership fence, a strict
// expected-revision check and a live authorization check.
//
// The first slice deliberately stays this narrow: one supported operation
// (append_fact in the human namespace), no Relay adapter, no service surface,
// no Binding authority records. Everything outside the supported surface is
// refused with a stable class instead of being simulated. The design contract
// lives in docs/design/enterprise-service.md §13; this package implements it.
//
// Transaction discipline: the caller's context bounds every statement and the
// COMMIT (and an internal budget bounds every operation even with an
// unbounded caller context); an uncertain commit outcome is reported as
// OutcomeUnknown and is never guessed, retried or reconciled here — Resolve
// can verify the final state under the same fence and authorization, and the
// caller decides.
//
// This kernel is an internal API for trusted producers (the Room core). It
// grants no end-user, HTTP or raw-event upload surface: Fact kinds are opaque
// to the kernel, and Binding, control and role mutations are refused rather
// than simulated.
package sqlstore
