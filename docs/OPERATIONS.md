# Operations

## Runtime shapes

| Entry | Ownership |
|---|---|
| Desktop | Native window/tray host; reuses a daemon or owns an embedded Service |
| `pairroom service` | Multi-Project/multi-Room Management Service |
| `pairroom serve` | Current-format standalone Room for development/diagnostics |
| `pairroom daemon` | Explicit OS background-Service management |
| `--mock` | Deterministic verification without vendor CLIs |

A desktop-owned **embedded Service** can host both Embedded and Native Rooms. Embedded Rooms own vendor adapters; Native Rooms own only the relay/listener and retain user-owned harness processes. See [Concepts](CONCEPTS.md).

From a source checkout, `make dev` stops an installed daemon, recovers a crash-stale lock only after its recorded PID is gone, and runs the current-tree Management Service. `make stop` is stop-only. Do not run competing owners against the same data root.

Service lock acquisition, stale recovery, and release are serialized across current processes by a root-scoped kernel guard. The PID/nonce file remains the ownership record, and an exited process releases the guard automatically. Older binaries do not honor this guard: stop previous owners and avoid concurrent mixed-version launches or recovery during upgrades.

All listeners require numeric loopback addresses. A token does not allow LAN, wildcard, or hostname binds. Remote access uses protected SSH local forwarding; treat access to that endpoint/bootstrap token as access to repositories, Agent credentials, and attachments.

## Desktop lifecycle

Desktop chooses one Service owner: an explicit validated `PAIRROOM_DESKTOP_URL`, an installed daemon, or an in-process embedded Service when no daemon is installed. The URL must be authenticated numeric loopback. An installed but unreachable daemon is an error, not permission to create a competing owner. Stale-lock recovery first proves the recorded PID exited; a live owner fails closed.

Explicit data-root/configuration or Mock options select their own Service instead of an unrelated default daemon; an explicit validated URL remains the strongest external-discovery override. A bundled CLI never authorizes daemon installation. **Settings → Desktop → Launch at login** changes only OS login registration. `make desktop-update` replaces binaries without changing user data or that registration. The opt-in **Check for updates** setting, off by default, only reads the latest GitHub release and shows a notice; it never downloads or installs anything ([details](../desktop/README.md#check-for-updates)).

| Action | Effect |
|---|---|
| Close desktop window | Hide to tray; do not stop Agents or Room runtimes |
| Launch another desktop instance | Focus the existing window |
| Quit with an external daemon | Exit the GUI; the daemon continues |
| Quit with an owned embedded Service | Stop Management mutations, drain owned runtime work, close stores, release the Service lock |
| Tray **Restart Service** (embedded only) | Drain the owned Service as on Quit, then start a replacement; Quit during the drain waits for it, and a failed drain keeps the lock and is retried by Quit instead of starting a competing Service |
| `pairroom daemon stop` | Gracefully stop the confirmed installed-daemon owner; do not delete a live lock or kill an unrelated process |
| `pairroom daemon open` | Open the Management URL recorded in the daemon's own log only after validating it as numeric loopback with a bootstrap token and probing it authenticated; a missing, unauthenticated, or non-loopback target is refused |

None of these controls can stop a user-owned Native harness. Windows daemon output goes to rotating logs rather than a persistent taskbar console; use `pairroom daemon logs`.

Desktop packages are produced for release tags or manual workflow dispatch, not PR/main verification builds. They attach to the same Release as CLI assets: `pairroom-cli-vX.Y.Z-…` and `pairroom-desktop-vX.Y.Z-…` (Windows setup executable, Linux `.deb`/`.AppImage`, macOS `.app.zip`, and `pairroom-desktop-vX.Y.Z-SHA256SUMS`). Production signing/notarization is not implied. See [Installation](INSTALLATION.md), [Desktop development](../desktop/README.md), and [release verification](../CONTRIBUTING.md#release-verification).

## Daily checks

Inspect the Service/Room state and actual repository evidence, not only chat text. Embedded exposes participant/native Turn state, delivery/processing, tool activity, and approvals. Native instead exposes binding/generation, registered collection, queued/delivering/unknown work, and observed wake activity; it cannot infer live presence or model completion from silence or receipts.

A quiet Runtime may be running a long command, compacting context, or performing an unexposed native step. A diagnostic error is not necessarily a terminal boundary. Native `handed_off` means stdout/ack, not completed work. Use [Native diagnostics](NATIVE_RELAY.md#recovery-and-review-surface) or [Troubleshooting](TROUBLESHOOTING.md) for the correct layer.

## Project, Archive, Delete

Management uses the desktop application icon in navigation and sign-in. The Project/Room list fills the sidebar above Service status and scrolls independently when it overflows. Primary actions use green buttons in light and dark themes across Management and Room views.

The Room context menu separates rename, **Close Room tab**, ordering, and confirmed **Archive Room**. It is available from Room rows/sidebar/tabs and through keyboard context-menu access. Closing a view and archiving are deliberately different operations.

| Action | Boundary |
|---|---|
| Close Room tab | Close only the view; no archive, deletion, suspension, order change, Room event, or native interruption. Reopen from navigation. |
| Unregister Project | Remove the Service registration, never the Git repository. Handle its remaining Rooms first. |
| Archive Embedded Room | Stop the current Agent Turn and suspend its Runtime; retain Room data and Binding ownership. |
| Archive Native Room | Hide it from the default list and prevent opening its surface until restored; retain data/ownership, but do not stop the original harnesses. |
| Permanent delete | Delete PairRoom-managed data only after current archive/Binding/runtime preconditions and explicit confirmation. |

Archive is not unbind, and closing a tab is not “stop work.” Follow returned UI/CLI/API preconditions; conflict responses are not permission to bypass lifecycle checks. Project unregister and Room deletion never imply deleting a repository. [API reference](API_REFERENCE.md#project-and-room-display-order) owns ordering and surface details.

## Capacity and idle reclaim

The active-runtime cap and capacity eviction apply to **Embedded Rooms that own vendor adapters**, not durable Room count. Native relay Rooms are exempt from capacity only: they do not consume a slot, queue behind Embedded capacity, or become capacity-eviction victims.

Idle suspension applies to every active Room Runtime, Embedded or Native. After the Service idle timeout (`--idle-timeout`, default 15 minutes) without use, a Runtime that is not busy, serving an in-flight relay request, or holding a pending wake is suspended without deleting Room history. Opening the Room or a later relay call such as `send` or `wait` activates it again; read-only diagnostics such as `relay doctor` do not. A starting Service also requests activation for suspended Rooms with pending work unless `resume_pending` is `false` ([Configuration](CONFIGURATION.md#service-runtime-policy)); a resumed Room that stays idle is suspended again after the idle timeout.

Reclamation suspends processes without deleting Room history and must not preempt a native Turn merely to free capacity. Embedded activation rebuilds only Room-owned FIFO entries that never crossed submission; accepted or uncertain input needs inspection rather than automatic replay. An uncertain cleanup retains its capacity claim instead of pretending to be suspended.

Embedded cleanup closes new-work admission before stopping adapters. New adapter starts/restarts are rejected, and already admitted startup calls must finish before their processes can be confirmed stopped. If startup or termination remains unproven, the Runtime stays Failed with its original error, capacity claim, and Event Log ownership. Its Store remains open for late process evidence. Only explicitly retryable cleanup is retried: archive/rename retries it once within that operation, suspend and activation can request a fresh bounded attempt, and the Service rechecks it with exponential backoff from one second to one minute. These attempts only stop remaining adapters; they never replay a Turn or restart an adapter. Successful cleanup releases ownership and capacity; an explicit activation may then start a new Runtime, including after a fatal failure, while background cleanup leaves the Room suspended.

In **Runtimes**, a retryable failure offers **Retry cleanup**; an attempt in progress shows **Stopping** and disables duplicate open actions. A persistent failure still blocks archive, rename, and deletion. Check the displayed error and residual work before restarting the Service. Summary append/sync failures and Store-close errors are not automatically replayed: after process cleanup, their terminal error is retained across repeated closes, including when the Store is already closed. Repair the storage problem and follow Service recovery rather than forcing a lifecycle write across uncertain persistence.

## Backup

Back up before incompatible upgrades, permanent deletion, data-root moves, manual integrity repair, or binding-policy changes. Stop/drain the relevant PairRoom owner. For Native consistency, also stop work explicitly in the original harnesses: archive/backup cannot do that for you.

`pairroom backup` / `restore` operate on **one Room data directory**, not the multi-Room Service root. For complete rollback, preserve the entire stopped Service root separately, including Agent pair profiles and navigation preferences, plus explicitly imported Room directories outside that root. The Git repository, vendor session stores, and Native workspace credentials/capability sidecars are separate and are not included in a Room archive.

Verification warns about each stored attachment no message references. Active Rooms reclaim such uploads after seven days ([Storage](STORAGE.md#attachment)), so these warnings normally describe recent unsent uploads; the backup still includes them.

Write backup and diagnostics outputs outside the source Room directory, including symlink aliases. Restore validates the file set, hashes, and complete gzip trailer before publishing the target; any file outside the Room layout (events, metadata, attachment manifests and their content) is rejected rather than restored. A successful compression command is not backup verification, and restored transport history does not prove external work stopped or completed. [Storage](STORAGE.md) owns integrity/replay details.

## Graceful shutdown

The Management listener first ends open Room event streams (in-app Room tabs reconnect after restart), then waits for in-flight requests up to `--shutdown-timeout` before draining Runtimes.

For Embedded, normal exit drains owned native work, settles projections, closes stores, and releases ownership. After a forced exit, only definitely pre-submission FIFO work is automatically rebuilt; unknown submission fails and accepted unfinished input is cancelled without replay.

Shutdown also makes one fresh cleanup attempt for an explicitly retryable Failed Runtime, with no concurrent duplicate close. It reports unresolved cleanup errors; a previously recovered failure does not make a later shutdown fail.

For Native, drain rejects new publications/claims while valid receipts for already released envelopes can settle. Shutdown does not terminate original sessions. Recovered unfinished delivery remains uncertain; inspect history and side effects before Retry. A hidden window or disconnected browser proves nothing about process completion.

## Attention notifications

Management may show a non-blocking build banner when the last authenticated stamped CLI differs from the running Service. It uses metadata on existing CLI/hook requests, not a disk inventory or a network update check. Both builds must carry valid stamps; one isolated report stays quiet. The same differing build must be observed again at least 30 seconds later without another valid build intervening. A matching build clears the banner; alternating CLI builds restart the stability window. Restart the owning Service explicitly: restart Desktop for a Desktop-owned Service, use `pairroom daemon restart` for an installed daemon, or restart the process that owns a standalone Service. No command is run by the banner.

When nobody is watching, the Service reports body-free attention notifications: an answer or `@user` publication for the human, a pending Embedded approval, a failed turn, a stall warning, uncertain Native delivery, a failed wake, or Native input left uncollected. They appear as toasts and optional system notifications in Management and Desktop, through `GET /api/v1/notifications`, and through an optional `notify_command` ([API](API_REFERENCE.md#attention-notifications)). A notification is a prompt to inspect, not a delivery, approval or completion record.

## Logs and diagnostics

Do not include API keys, Authorization headers, or absolute attachment paths in logs. Review arbitrary user/tool content before sharing any export. A useful report includes the PairRoom build, OS/entry type, **Room host mode**, selected CLI versions, relevant Room/message/Turn IDs where applicable, first error/system notices, and a redacted minimal reproduction.

Management **Settings → Diagnostics** has environment checks and separately consented live runtime tests. Native Room diagnostics/`relay doctor` inspect relay observations without calling a model. These are different from a full Room diagnostics archive; [Support](../SUPPORT.md) and [Troubleshooting](TROUBLESHOOTING.md) describe safe reporting.
