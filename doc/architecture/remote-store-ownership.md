# Remote store ownership

## CTR-REMOTE-001 — Remote store ownership

The manager owns one canonical remote-skill store below its existing user cache
root. The store is a direct discovery root owned only by skills-mgr; it is not
copied, linked, or synchronized into an agent-owned skill root.

Each persisted entry is identified by catalog provider plus provider skill ID
and records the validated declared skill name, provider refresh locator, and
last successful fetch time. A different provider identity or declared name
must not silently replace an existing entry.

This contract supports `REQ-SYNC-001` and `REQ-SYNC-002`.
