<!-- pairroom-relay:reference -->
# Enterprise Native relay

Read this only for an enterprise origin/deployment, enterprise Binding, or requested enterprise login, admission, renewal or recovery. Keep the user's exact enterprise route; do not use local/LAN fallback or local preflight create guidance.

## Login and bind

Use `pairroom enterprise --help` and `pairroom relay bind --help` for the installed version's flags. The human supplies the exact HTTPS origin and deployment ID. Device login is foreground work with browser approval: `pairroom enterprise login --service-origin <HTTPS-origin> --deployment <id>`. Hooks never initiate login, refresh or renewal.

Bind to the same existing Room and exact selectors using `--enterprise-service <HTTPS-origin> --deployment <id>`. The Owner approves the exact request; the original endpoint repeats its original bind to finish. Preserve the original attempt identity on uncertainty. Binding credential renewal is explicit foreground `pairroom relay bind --renew-credential` from the original session; never silently replace or recreate the Room.

## Files and uncertain effects

For staged files, use a stable `--id`. Keep uncertain IDs and bytes; resolve the original operation instead of reprinting or republishing it. Retry only a proven missing upload, with the same object and bytes. Verify before Claim; ACK after stdout. CLI-managed state and credentials stay private.

Local logout or abandonment retains recovery evidence and does not revoke server authority. Use the installed command's help and original operation identity for explicit recovery; do not clear private journals to start over.
