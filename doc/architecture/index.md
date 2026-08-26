---
pjdoc:
  version: 1
  kind: architecture
  scope: root
  status: draft
  revision: ARCH-4
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

This contract supports `REQ-SYNC-001` and `REQ-SYNC-002`.


## CTR-REMOTE-002 — Provider content boundary

The existing skills.sh and SkillsMP registry owners also own conversion of
their catalog records into complete remote file sets:

- skills.sh content comes from a `git clone --depth 1` of the catalog source
  repository. Extra ID path segments are searched as a source subdirectory;
  otherwise the named skill is located from that repository the same way
  `npx skills add <source> --skill <name>` is.
- SkillsMP content comes from the GitHub location supplied in its catalog
  record and is retrieved with `git clone --depth 1`.

Provider results are untrusted data. Before they reach the store, the manager
must reject absolute or escaping paths, non-regular entries, duplicate paths,
oversized responses, and any tree whose root `SKILL.md` does not parse to the
catalog skill's name. Provider and store operations must not execute downloaded
content.

This contract supports `REQ-SYNC-002` and `REQ-SYNC-003`.

## CTR-REMOTE-003 — Atomic refresh and selection ordering

One manager ensure path validates a complete provider result in a temporary
sibling entry and atomically replaces the prior entry only after every file and
metadata record is durable enough to close successfully. Interactive enable
and explicit synchronization call that path. Synchronization stages project
metadata reconciliation in memory, persists it only after all content work
succeeds, and leaves both selection locks unchanged on failure or cancellation.

Disabling changes only the existing project lock. The daemon enumerates store
metadata and calls the same refresh path for stale entries; it does not inspect
or mutate project locks.

This contract supports `REQ-SYNC-002` and `REQ-SYNC-003`.


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

This contract supports `REQ-SYNC-002`.

## CTR-REMOTE-005 — Local patch layer

The remote store owns one stable `.patch` sidecar per remote identity under
`$HOME/.skills-mgr/skills/.remote-patches/`. This global manager-home location
is outside the refetchable remote cache, so a tracked patch can be restored
before its provider content is installed. The patch records the fetched
`SKILL.md` digest it was based on and the expected patched-result digest, so
changed provider instructions and structurally damaged patches cannot silently
produce another result. The TUI edits a private temporary copy of the currently
layered `SKILL.md` and atomically replaces the sidecar only after the editor
succeeds. The store compares that starting layer again under its exclusive lock
before publishing, so concurrent editors cannot overwrite an intervening edit.
It never writes into provider content.

The `get` owner applies the sidecar only to a remote skill's `SKILL.md` before
frontmatter stripping and line-range selection. Patch parsing or application
is all-or-nothing: failure discards any partial result, writes the original
provider content through the normal output path, and returns an error for the
CLI to report on stderr with nonzero status. Discovery, placeholders, and
script execution continue to use validated provider content. Refresh preserves
the sidecar, while uninstall removes it with the remote entry.

This contract supports `REQ-LAYER-001`.

## CTR-SYNC-001 — Selection identity and orchestration

`lock.go` owns the executable `.skills-mgr.json` schema and compatibility
decoding. Its structured skill selection embeds the existing remote reference
shape owned and validated by `remote_skill.go`; it must not derive identity
from a mutable catalog search or duplicate provider-specific parsing. During a
revision-1 rewrite, the manager matches selected skill names directly to the
canonical store records; normal discovery precedence must not suppress remote
identity reconstruction. Project-selected identities retain explicit
enablement, while globally inherited identities are written without an enabled
override.

The `sync` command first matches project and inherited global skill names to
canonical persisted remote records, independently of discovery precedence.
It stages missing project metadata, filters effectively enabled entries through
the global/project overlay, and passes each enabled reference to the existing
store ensure path. After all ensures succeed, it atomically persists the staged
project metadata without changing enabled overrides.

This contract supports `REQ-SYNC-001`, `REQ-SYNC-002`, and `REQ-SYNC-003`.
