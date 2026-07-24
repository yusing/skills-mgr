---
pjdoc:
  version: 1
  kind: architecture
  scope: root
  status: draft
  revision: ARCH-1
  files:
    []
---
# skills-mgr architecture

## CTR-REMOTE-001 — Remote store ownership

The manager owns one canonical remote-skill store below its existing user cache
root. The store is a direct discovery root owned only by skills-mgr; it is not
copied, linked, or synchronized into an agent-owned skill root.

Each persisted entry is identified by catalog provider plus provider skill ID
and records the validated declared skill name, provider refresh locator, and
last successful fetch time. A different provider identity or declared name
must not silently replace an existing entry.

This contract supports `REQ-REMOTE-001`, `REQ-REMOTE-002`, and
`REQ-REMOTE-003`.

## CTR-REMOTE-002 — Provider content boundary

The existing skills.sh and SkillsMP registry owners also own conversion of
their catalog records into complete remote file sets:

- skills.sh content comes from its skill-detail API using the catalog ID.
- SkillsMP content comes from the GitHub location supplied in its catalog
  record and is retrieved with `git clone --depth 1`.

Provider results are untrusted data. Before they reach the store, the manager
must reject absolute or escaping paths, non-regular entries, duplicate paths,
oversized responses, and any tree whose root `SKILL.md` does not parse to the
catalog skill's name. Provider and store operations must not execute downloaded
content.

This contract supports `REQ-REMOTE-001` and `REQ-REMOTE-004`.

## CTR-REMOTE-003 — Atomic refresh and selection ordering

One manager refresh path validates a complete provider result in a temporary
sibling entry and atomically replaces the prior entry only after every file and
metadata record is durable enough to close successfully. Interactive enable
changes the existing project lock only after that path succeeds. Failure
retains both the last-known-good entry and the prior project lock.

Disabling changes only the existing project lock. The daemon enumerates store
metadata and calls the same refresh path for stale entries; it does not inspect
or mutate project locks.

This contract supports `REQ-REMOTE-002`, `REQ-REMOTE-003`, and
`REQ-REMOTE-004`.

## CTR-REMOTE-004 — Discovery and TUI integration

The existing manager discovery owner reads the canonical remote store after
the existing agent and plugin roots, preserving their current precedence. A
persisted remote entry participates in the current name-based project
selection only when no higher-precedence discovered skill already owns that
name.

Catalog presentation carries provider identity and refresh locator through the
existing asynchronous TUI message path. Remote rows derive enabled state from
the current project selection; a completed toggle reloads discovery and
selection together before rendering success.

This contract supports `REQ-REMOTE-001` and `REQ-REMOTE-005`.
