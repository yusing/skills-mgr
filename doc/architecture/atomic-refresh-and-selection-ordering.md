# Atomic refresh and selection ordering

## CTR-REMOTE-003 — Atomic refresh and selection ordering

One manager ensure path validates a complete provider result in a temporary
sibling entry and atomically replaces the prior entry only after every file and
metadata record is durable enough to close successfully. Interactive enable
and explicit synchronization call that path. Synchronization stages project
metadata reconciliation in memory, persists it only after all content work
succeeds, and leaves both selection locks unchanged on failure or cancellation.

Disabling changes only the existing project lock. The on-demand background
runner enumerates store metadata and calls the same refresh path for stale
entries. It does not inspect or mutate project locks.

This contract supports `REQ-SYNC-002` and `REQ-SYNC-003`.
