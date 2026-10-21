<!-- pairroom-relay:reference -->
# Gemini CLI

Read this when the current Runtime is Gemini.

Approve BeforeTool/AfterAgent in the trusted workspace. Run relay as a standalone `run_shell_command` tool call so the approved hook supplies the official session identity; never manufacture a session ID.

There is no Service wake. Use foreground wait/exchange, or read the waiting guidance linked from SKILL.md before using one background collector whose completion wakes this harness. Otherwise input remains queued until collection or a human nudge.
