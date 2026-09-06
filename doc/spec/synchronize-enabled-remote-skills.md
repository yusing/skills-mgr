# Explicitly synchronize enabled remote skills

## REQ-SYNC-002 — Explicitly synchronize enabled remote skills

`skills-mgr sync` shall reconcile missing remote references into the current
project selection by matching project and inherited global skill names to
identities recorded in the global selection or the current user's canonical
persisted remote-store records. Conflicting identities for the same skill name
shall fail synchronization. Explicit project entries shall retain enabled
state. Inherited records shall omit enabled state so they continue following
the global selection. Normal discovery precedence shall not suppress this
metadata reconciliation.

After reconciliation, each effectively enabled remote reference shall exist as
a fresh, valid copy in the current user's remote-skill store. The command shall
use the provider named by the record directly rather than discover or search
for the skill in a catalog. It shall not fetch effectively disabled records.
Existing commands and TUI startup shall not perform this synchronization
implicitly.

The on-demand background runner shall refresh only identities already present
in the store. It shall not inspect project selections or fetch a remote skill
merely because its identity appears in configuration.

Acceptance examples:

- On a machine with an empty remote store, `skills-mgr sync` fetches each
  effectively enabled project remote record and makes it available to existing
  `list`, `get`, and `run` behavior.
- With an empty remote store, an enabled remote identity recorded only in the
  global selection is fetched by `skills-mgr sync` when the current project
  inherits it.
- A disabled global remote identity is not fetched by `skills-mgr sync`.
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
