# Room batch lifecycle validation — 2026-08-18

## Audited baseline

- Branch: `main`
- Commit: `c3d9db70693ef53f8cf51768df5095cf34b22ed1`
- Baseline already includes safe empty-Project unregister and Project path refresh but has no permanent Room-removal route.
- This package is cumulative over the uploaded Room-deletion package; it replaces typed Room-ID cleanup confirmation and adds batch lifecycle management.

## Implemented interaction contract

- Room cleanup never asks the operator to type a Room ID.
- Every visible Room can be selected. The same persistent selection supports batch archive for active Rooms and batch permanent cleanup for archived Rooms.
- Group selection can target the current Project or the current filtered Project result. A selection contains at most 100 Rooms; excess candidates are skipped with a visible count.
- Successful archive items remain selected and archived Rooms are revealed, enabling an immediate archive → cleanup workflow.
- Permanent cleanup shows the selected count and a bounded name preview and requires one explicit irreversible-data-loss checkbox.
- Successful cleanup items leave the selection. Failed existing Rooms remain selected for retry after their busy/invalid state is resolved.
- Project unregister still requires the exact durable Project ID; this change deliberately does not weaken that separate operation.

## API contract

### Batch archive

```http
POST /api/v1/rooms/batch-archive
Content-Type: application/json

{"room_ids":["room-a","room-b"]}
```

- Accepts 1–100 submitted IDs.
- Validates the complete array and rejects blank IDs, surrounding whitespace and oversized input before mutation.
- De-duplicates exact IDs in first-seen order.
- Returns ordered per-item `archived`, `already_archived` or `failed` results.
- Treats an already archived Room as an idempotent success.
- Does not interrupt an active Turn. A busy Room returns `runtime_busy` immediately for that item so later batch items can continue.

### Batch permanent cleanup

```http
POST /api/v1/rooms/batch-delete
Content-Type: application/json

{
  "room_ids":["room-a","room-b"],
  "acknowledge_data_loss":true
}
```

- Uses the same validation, limit, first-seen de-duplication and ordered partial-success model.
- Requires `acknowledge_data_loss: true` but no typed ID.
- Only archived Rooms are eligible; active Rooms return `room_not_archived`.
- A valid batch returns `200 OK` even when individual Rooms fail. One item failure never rolls back previously completed items.
- `DELETE /api/v1/rooms/{room}` remains available and requires only `{"acknowledge_data_loss":true}`.

Example partial result:

```json
{
  "submitted": 3,
  "processed": 2,
  "succeeded": 1,
  "failed": 1,
  "duplicates_ignored": 1,
  "results": [
    {
      "room_id": "room-a",
      "status": "deleted",
      "removal": {
        "room_id": "room-a",
        "project_id": "project-a",
        "data_disposition": "deleted"
      }
    },
    {
      "room_id": "room-b",
      "status": "failed",
      "code": "runtime_busy",
      "error": "..."
    }
  ]
}
```

## Runtime and data safety

- Single-Room archive retains its existing wait-for-safe-Turn behavior.
- Batch archive uses the non-interrupting immediate suspend boundary so one busy Room cannot stall the entire request.
- Permanent cleanup closes Runtime admission before Registry deletion. Queued activation is cancelled, idle Runtime is closed, and busy or cleanup-uncertain Runtime is rejected.
- PairRoom-managed Room data is quarantined, checkpointed out of the Registry, then physically erased. Binding ownership is released.
- Explicitly imported external Room directories are unregistered and retained.
- Git worktrees and vendor Claude Session/Codex Thread data remain outside the deletion boundary.
- Removing the final Room makes the existing empty-Project unregister operation available.
- `POST /api/v1/maintenance/room-deletions/retry` retries only already-committed physical cleanup.

## Crash consistency

The managed-data sequence remains durable intent → atomic same-filesystem quarantine rename → post-rename Event Log/identity/lifecycle verification → Registry checkpoint → committed marker → recursive cleanup.

| Durable evidence | Startup action |
|---|---|
| Prepared intent and trusted checkpoint still owns Room | Restore Room directory |
| Prepared intent and trusted checkpoint omits Room | Complete cleanup |
| Committed marker | Complete cleanup even if checkpoint later becomes unreadable |
| Missing/corrupt/untrusted checkpoint and no committed marker | Restore conservatively |
| Identity mismatch, unknown entry, symlink, or non-directory quarantine entry | Refuse startup and preserve evidence |
| Logical deletion committed but recursive cleanup failed | Keep Room deleted; expose `cleanup_pending` and retry later |
| Registry fail-closed with uncommitted prepared data | Never erase through maintenance retry; defer to startup recovery |

## Local verification completed

The final Go overlay was compiled with the complete local `internal/service` package harness and passed:

```text
go test -count=1 -timeout=180s ./internal/service
go test -race -count=1 -timeout=240s ./internal/service
go vet ./internal/service
```

The focused batch archive/removal API tests passed 20 consecutive runs. Coverage includes de-duplication, stable ordering, idempotent already-archived results, busy/missing partial failure, validation-before-mutation, acknowledgement enforcement, successful final-Project unblocking, and the existing crash-consistent deletion core.

The delivery package additionally passed:

- `gofmt` checks for every changed Go file;
- Python compilation of `apply.py` and `tools/frontend_transform.py`;
- exact-once validation for 17 JavaScript and 2 HTML transformation anchors;
- required output-marker and banned typed-Room-ID/native-dialog/Web-Storage checks;
- a composed valid JavaScript fixture covering all 17 replacements before and after transformation, both passing `node --check`;
- synthetic clean-Git applicator E2E with exact hash, clean-tree, check-only, 14-path application and output-marker checks;
- injected mid-application write failure with byte-for-byte rollback of all 14 planned paths;
- the standalone 12-path core review patch applying cleanly and matching every direct overlay file;
- clean package inventory, internal SHA-256 verification and ZIP/TAR.GZ integrity checks.

## Gates still requiring the complete exact-baseline checkout/release environment

The sandbox could not independently materialize the complete public repository checkout and transform the full Management Shell asset. `apply.py --verify` therefore deliberately enforces `node --check` on the complete transformed `management.js` in the actual target checkout.

The remaining release gates are:

- exact-checkout `python3 apply.py ... --verify --smoke`;
- repository-wide `make check`;
- deterministic `make smoke`;
- real browser desktop/mobile visual and accessibility smoke;
- authenticated current Claude Code/Codex native E2E;
- cross-platform binaries, SBOM, provenance, signing and release-asset verification.
