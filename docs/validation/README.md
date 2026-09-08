---
status: historical
kind: validation-records
---

# Historical validation records

These retained JSON reports belong to older release snapshots. **They are not current test results, a native Runtime compatibility matrix, or evidence that today's source passes.** Their filenames identify their historical scope; do not update their contents to make them appear current.

| Snapshot | Retained records |
|---|---|
| v0.2.0 | [Mock E2E](v0.2.0-mock-e2e.json), [restart E2E](v0.2.0-restart-e2e.json), [UI static check](v0.2.0-ui-static-check.json) |
| v0.3.0 | [Doctor](v0.3.0-doctor.json), [Mock E2E](v0.3.0-mock-e2e.json), [restart E2E](v0.3.0-restart-e2e.json), [rich conversation E2E](v0.3.0-rich-conversation-e2e.json) |
| v1.0.0 | [Doctor](v1.0.0-doctor.json), [Mock E2E](v1.0.0-mock-e2e.json) |

Read [History provenance](../../HISTORY_PROVENANCE.md) before using early commit references: some histories were reconstructed and an old recorded prefix may not identify an object in this repository.

For current verification, use [Contributing](../../CONTRIBUTING.md) and the CI results for the exact commit being reviewed. Distinguish unit/race tests, build checks, browser fixtures, real-browser Mock Service tests, and authenticated native E2E. A probe or Mock success is not a real coding-agent test, and none of these old reports is a comparative model-quality/cost benchmark.

New one-off run output belongs with its CI run or PR evidence, not copied into this directory as another undocumented source of truth. Current recovery and schema behavior belongs in [Storage](../STORAGE.md) and [Upgrading](../UPGRADING.md).
