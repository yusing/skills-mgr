# Atomic refresh and selection ordering

## CTR-REMOTE-003 — Atomic refresh and selection ordering

One manager ensure path validates a complete provider result in a temporary
sibling entry and atomically replaces the prior entry only after every file and
metadata record is durable enough to close successfully. Interactive enable
and explicit synchronization call that path. Synchronization stages project
metadata reconciliation in memory, persists it only after all content work
succeeds, and leaves both selection locks unchanged on failure or cancellation.

Disabling changes only the existing project lock. Automatic daemon cycles
enumerate store metadata and call the same refresh path for stale entries. The
explicit daemon sync command first ensures unconditionally enabled identities
from the global lock, then refreshes stale store entries. Neither path inspects
or mutates project locks.

This contract supports `REQ-SYNC-002` and `REQ-SYNC-003`.
