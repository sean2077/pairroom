# Pre-LAN local compatibility fixture

These Native and Embedded Rooms were created through the real Service API of
unmodified v5.13.1 at commit `8da3842c6a3c0216730ad2d09f306705daa10a4b`, using
Mock provisioning with automatic start disabled. Store 12, provisioning 5 and
checkpoint 3 are original output, not relabelled new-format records.

Only test-owned absolute directories and their derived Project ID are replaced
by placeholders. Tests substitute fresh local paths and check that the upgraded
reader retains both Rooms and every original Room file byte. The derived
checkpoint may advance to schema 4; Room Event Logs and metadata stay unchanged.
