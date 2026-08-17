# Project Management Validation Record — 2026-08-17

Baseline: `main` at `030d0321ebdafb42e09632a439d1cf6f71f8553a`.

This record covers the Project path-refresh and safe-unregister increment. It does not replace the repository-wide release record or claim a native authenticated Claude Code/Codex run.

## Implemented contract

- `POST /api/v1/projects/{project}/refresh` re-resolves the canonical root and durably projects `available` plus `diagnostic` without migrating Project identity.
- `DELETE /api/v1/projects/{project}` requires an exact `confirm_project_id` body and only unregisters Projects with zero Rooms.
- Active and archived Rooms both block unregister with a structured `409 project_has_rooms` response.
- Unregister and Room provisioning share the Registry mutation lock, so one operation wins atomically and no orphaned Room can be created.
- Successful unregister removes only the Registry entry. It does not delete the Git worktree, Room data, Event Logs, attachments, or vendor Session/Thread state.
- The Management Shell gates controls through `project_refresh` and `project_removal`, exposes quick path revalidation, and uses an in-page typed-confirmation dialog rather than native prompt/confirm or Web Storage.

## Verification completed in the implementation environment

```text
gofmt -w internal/service/management.go \
  internal/service/management_test.go \
  internal/service/project_management.go \
  internal/service/project_management_test.go

go test -count=1 ./internal/service                 PASS
go test -race -count=1 ./internal/service           PASS
go vet ./internal/service                           PASS
python3 -m py_compile tools/frontend_transform.py   PASS
exact-anchor frontend transformation contract       PASS
node --check extracted inserted JavaScript contract PASS
```

Focused tests cover:

- persistence across Registry restart and re-registration of the same worktree;
- byte-for-byte preservation of a worktree marker;
- rollback after pre-publication checkpoint failures for both unregister and path refresh;
- rejection when an active or archived Room remains;
- serialization against concurrent Room provisioning;
- unavailable-path persistence and recovery;
- exact API confirmation, successful `204`, and structured conflict details.

## Release gates still required on the complete repository

Run the package applicator with `--verify`, then complete the normal release checklist. In particular:

```text
make check
make smoke
```

Also perform a browser visual/accessibility smoke against the transformed real embedded assets, the current authenticated Claude Code/Codex native acceptance test where available, and the cross-platform release artifact/SBOM/provenance verification. No unperformed gate is represented here as passed.
