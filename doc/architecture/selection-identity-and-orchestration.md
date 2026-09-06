# Selection identity and orchestration

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
remote identities in the global selection and canonical persisted records,
independently of discovery precedence. A disagreement between those identity
sources fails as a conflict. It stages missing project metadata, filters
effectively enabled entries through the global/project overlay, and passes each
enabled reference to the existing store ensure path. After all ensures succeed,
it atomically persists the staged project metadata without changing enabled
overrides.

The on-demand background runner refreshes only stale persisted store records
and does not inspect project or global selection locks.

This contract supports `REQ-SYNC-001`, `REQ-SYNC-002`, and `REQ-SYNC-003`.
