---
pjdoc:
  version: 1
  kind: spec
  scope: root
  status: draft
  revision: SPEC-2
  files:
    []
---
# skills-mgr product specification

This draft specifies the increment described by
[`doc/brief.md`](../brief.md).

## REQ-SYNC-001 — Persist remote selection identity

When a remote catalog skill is enabled or disabled in a project, its selection
shall record its enabled state and stable remote reference: provider,
provider-specific ID, skill name, and provider locator. A local selection shall
record only its enabled state.

When a schema-revision-1 project file is rewritten as revision 2, migration
shall initialize remote references by matching selected skill names to canonical
persisted remote-store records, independently of higher-precedence discovery
roots. Project overrides shall retain their enabled state. Inherited records
shall omit enabled state so their effective state continues to follow the
global selection. Existing schema-revision-1 files whose skill values are
booleans shall remain readable, and migration shall not invent identity for
entries that have no persisted remote-store record.

Acceptance examples:

- Enabling a skills.sh or SkillsMP result writes enough identity for another
  machine to fetch that exact provider entry without searching a catalog.
- Disabling the remote skill retains its identity while changing its enabled
  state to false.
- Migrating a project records identity for an inherited remote without adding
  an enabled override.
- Toggling a local skill writes structured enabled state without remote
  metadata.
- An existing boolean-only project file continues to control local and already
  persisted skills and is upgraded on its next selection change.

## REQ-SYNC-002 — Explicitly synchronize enabled remote skills

`skills-mgr sync` shall reconcile missing remote references into the current
project selection by matching project and inherited global skill names to the
current user's canonical persisted remote-store records. Explicit project
entries shall retain enabled state. Inherited records shall omit enabled state
so they continue following the global selection. Normal discovery precedence
shall not suppress this metadata reconciliation.

After reconciliation, each effectively enabled remote reference shall exist as
a fresh, valid copy in the current user's remote-skill store. The command shall
use the provider named by the record directly rather than discover or search
for the skill in a catalog. It shall not fetch effectively disabled records.
Existing commands and TUI startup shall not perform this synchronization
implicitly.

Acceptance examples:

- On a machine with an empty remote store, `skills-mgr sync` fetches each
  effectively enabled project remote record and makes it available to existing
  `list`, `get`, and `run` behavior.
- A fresh persisted copy causes no provider request.
- A stale persisted copy is refreshed using existing refresh semantics.
- A project whose prior v2 migration omitted identities is repaired from its
  persisted remote store.
- An inherited remote record receives metadata without an enabled override and
  follows global enablement.
- Effectively disabled remote records receive metadata but cause no provider
  request.
- Running `list`, `get`, `run`, or the TUI against a missing referenced remote
  skill does not download it.

## REQ-SYNC-003 — Preserve trust boundaries and report failures

Synchronization shall use the existing remote provider, content validation,
resource limits, safe-path checks, and atomic persistence behavior. It shall
never execute downloaded content or write it into agent-owned skill roots.

For every successfully synchronized remote skill, the command shall write its
name to standard output. If any selected identity is invalid, unavailable,
conflicts with another persisted skill, or produces invalid content, the
command shall return an error identifying that skill. Previously valid content
and both selection files shall remain unchanged.

Acceptance examples:

- Unsafe paths, excessive content, invalid `SKILL.md`, or provider failure
  produces a non-zero result without partial project-selection updates.
- If an earlier skill succeeded before a later skill failed, its valid
  user-level persisted copy may remain; the configuration is unchanged.
- Successful output contains one line per synchronized skill and no downloaded
  file is executed.
