# Native v8 compatibility fixture

Captured from PairRoom v5.15.0, commit `9d22ceca3a557c241605a74c44d11177f8204291`, using the unmodified CLI built from that revision. This is historical input for the Native v9 reader, not a current creation example.

The fixture contains a local Native Room and a LAN Native Room, with Store 13, provisioning 6 and Registry checkpoint 4. The local Room selected Claude Code and Codex at creation; the LAN Room selected its Grok owner and reserved the remote slot without a Runtime. Each owner then bound a synthetic session through the Management API. No vendor CLI was started.

Capture used `pairroom service --mock --no-browser --resume-pending=false` with an isolated data root and private configuration directory, then `POST /api/v1/projects`, `POST /api/v1/projects/{project}/rooms`, and `POST /api/v1/rooms/{room}/native-bindings/slot1`. The separate LAN listener was enabled on numeric loopback for LAN creation and disabled before graceful shutdown.

Only data-root, workspace and derived Project-ID values are replaced by placeholders. Room IDs, event order, schema versions, selections and binding facts are original writer output. Credentials are synthetic fixture values; Service tokens, TLS keys and native identity reservations are not included.
