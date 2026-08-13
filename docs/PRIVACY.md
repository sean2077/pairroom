# PairRoom Privacy Model

PairRoom is local-first software. It has no PairRoom-operated cloud service, telemetry collector, account system, or model proxy.

## Data stored locally

A room can store:

- user prompts and Agent final responses;
- message routing, replies, delivery and processing state;
- bounded tool, command, plan, diff, usage, and error summaries;
- approval requests and decisions;
- repository paths and Git metadata;
- uploaded or Agent-generated raster images;
- Claude session IDs and Codex thread IDs.

The default data directory uses private permissions where the operating system supports them.

## Data sent to model vendors

PairRoom launches the user's official Claude Code and Codex installations. Those Harnesses determine what repository text, images, tool results, MCP output, and conversation context are sent to their respective providers. PairRoom does not intercept, re-encrypt, anonymize, or change vendor retention policies.

Review the vendor terms and your organization policy before using private code or images.

## Browser data

The browser stores non-secret UI preferences such as theme, composer draft, unread cursor, and selected routing intent in local storage. The long-lived API bootstrap token is not stored in Web Storage. A non-loopback browser uses a short-lived HttpOnly session cookie and an in-memory CSRF token.

## Exports and backups

Transcript exports contain conversation content and attachment metadata. Forensic JSON exports can include the bounded Inspector event tail. Room backups contain the complete transcript and attachment bytes. Diagnostics are redacted by default but should still be reviewed before sharing.

## Remote resources

PairRoom does not automatically fetch remote Markdown images. Normal links are opened only after user action and are then handled by the browser directly.

## Deletion

PairRoom has no remote copy to delete. Stop the daemon and securely remove the room data directory and backups according to the storage medium and organizational policy. Deleting a repository does not automatically delete its PairRoom room directory.
