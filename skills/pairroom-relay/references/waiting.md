<!-- pairroom-relay:reference -->
# Waiting and reachability

Read this before background/unattended waiting or when diagnosing idle wake.

Keep one collector only: resolve it before another collector. If tracked completion wakes the harness without idle model turns, keep one bound wait (`pairroom relay wait --timeout 0` until cancelled), and rehang on expiry until unbound or a human stops it; quiet is not stop.

Poll-only tools: no idle background wait/poll. Unseen stdout may take input and suppress Service wake. Bounded Stop park may receive queued input; wake needs a live local observer. No collector/hook/observer or failed wake means input stays queued for collection or a human nudge.

For an imminent reply during work, use one foreground wait/exchange within the tool limit; an empty timeout ends the turn. The default hour is a transport limit, not the harness lifetime.

Wake is rate-limited and fail-closed. Recheck only unattempted cooldown work; never automatically retry reserved effects. Never execute printed vendor wake commands. Grok and Gemini have no Service wake.
