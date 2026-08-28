# Persist remote selection identity

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
