# Enterprise Linux deployment

These files are a local deployment candidate. Use the matching source revision and the measured acceptance report; a successful image build does not establish production signing, private-provider acceptance, vendor model acceptance or HA. The service runs Native coordination only. Endpoint model credentials and sessions stay on endpoints.

Build from the repository root:

```sh
docker build -f deploy/enterprise/Dockerfile -t pairroom-enterprise:local .
```

Copy `enterprise.example.json` to operator-owned configuration and set the exact public HTTPS origin, organization, deployment, PostgreSQL schema and configured OIDC connection. Use PostgreSQL 17 with a role restricted to the intended database/schema. Configure the IdP callback as `<public_origin>/enterprise/auth/callback`; this CLI selects RS256 ID-token verification and the explicitly configured `basic` or `post` client authentication method. The example has no admitted subjects; bootstrap names one explicit administrator, and subsequent admissions are explicit administrator actions. Keep actual secret values in an owner-readable environment file, using the named references in JSON. Neither secrets nor real identity lists belong in the image, repository or diagnostics. On Linux, give the configuration directory mode 0700 and its files mode 0600 under UID/GID 10001, including the TLS key. The image runs as UID/GID 10001 and includes no vendor CLI or private provider bridge. A fresh Docker data volume inherits that owner and mode 0700 from the image; a preexisting bind directory needs the operator to establish the same ownership and permissions before startup.

Mount configuration and TLS read-only at `/run/pairroom`, and one persistent writable volume at `/var/lib/pairroom`. The example keeps 256 accepted connections and 32 concurrent business requests, with separate bounded replay/object work; lower configured limits refuse new work instead of authorizing deletion of retained history. Restrict the published host interface to the operator-selected interface and apply its firewall policy. The development example publishes loopback:

```sh
docker volume create pairroom-enterprise-data
docker run --rm --name pairroom-enterprise-init --env-file /private/enterprise.env --mount type=bind,src=/private/config,dst=/run/pairroom,readonly --mount type=volume,src=pairroom-enterprise-data,dst=/var/lib/pairroom pairroom-enterprise:local enterprise init --config /run/pairroom/enterprise.json --bootstrap-subject SYNTHETIC_EXAMPLE_ADMIN
docker run --name pairroom-enterprise --restart unless-stopped --env-file /private/enterprise.env --mount type=bind,src=/private/config,dst=/run/pairroom,readonly --mount type=volume,src=pairroom-enterprise-data,dst=/var/lib/pairroom --publish 127.0.0.1:8443:8443 pairroom-enterprise:local
```

For systemd, create the selected `pairroom` system User/Group, install the built CLI and operator configuration, create `/var/lib/pairroom` with that owner's mode 0700, and install the example unit through the normal operator process. The unit does not create or initialize a deployment. Run explicit `enterprise init` before starting it. Container/systemd examples are alternatives; only one process may own a deployment. No daemon or login registration is installed by the CLI on launch.

Direct TLS requires a valid certificate for the exact public hostname. For an external TLS terminator, omit the direct certificate/key only when `trusted_proxy_prefixes` names exact immediate proxy networks, preserve Host, and send exactly `X-Forwarded-Proto: https`. Forwarded User/host/client-IP headers confer no identity. Never publish the Local Management listener instead of this authenticated enterprise entrypoint.

`GET /enterprise/health/live` reports process availability. `GET /enterprise/health/ready` checks supported schema, current ownership, completed ownership-epoch recovery and the durable restore barrier. Existing sessions may remain usable during an IdP outage, so readiness does not assert new-login success. Shutdown closes new admission and drains HTTP work before releasing ownership; it never stops user-owned endpoint harnesses. The selected engineering bounds limit memory, new work and persistent records; they are not a measured throughput promise. Archive retains facts and Binding reservations, while limiting new work. Operator storage monitoring remains necessary for append-only facts, receipts and audit.

Back up offline after stopping the service owner. Keep the original backup/restore operation ID on failure or uncertainty. The bundle is owner-private and contains authoritative identity/control state and Room contents. Move it only through an authorized protected channel.

```sh
pairroom enterprise backup --config /etc/pairroom/enterprise.json --output /var/lib/pairroom/backups/backup-one --id backup-one
pairroom enterprise restore --config /etc/pairroom/restore-target.json --source /private/backup-one --id restore-one
pairroom enterprise resolve-restore --config /etc/pairroom/restore-target.json --id restore-one --manifest-hash ORIGINAL_MANIFEST_SHA256
pairroom enterprise complete-restore --config /etc/pairroom/restore-target.json --id restore-one --manifest-hash ORIGINAL_MANIFEST_SHA256
```

Restore selects a pristine target schema and new private object root; it leaves the source untouched and does not run backup-provided SQL. The original deployment and organization must match. A fresh restore epoch permanently invalidates old sessions/grants and unconsumed approvals; old unreferenced drafts cannot regain authority through a fresh login. History and referenced objects survive. The SQL restore barrier survives loss of local files and prevents public startup until object verification, fresh ownership-epoch delivery recovery and explicit completion finish. Repeating an uncertain import resolves its original ID; it does not import again. Failed backup keeps retention barriers and publishes no complete manifest; after inspecting it, the operator can explicitly end that exact barrier with `enterprise backup-end --config ... --id ...`. Preserve partial evidence until inspection is complete.

Use a new isolated target for rehearsal. These are offline recovery procedures, with expected service interruption. They do not provide automatic failover, two writable database primaries, zero data loss or an HA RPO/RTO guarantee. [Operations](../../docs/OPERATIONS.md) and the [enterprise design](../../docs/design/enterprise-service.md) own the behavior and remaining acceptance boundaries.
