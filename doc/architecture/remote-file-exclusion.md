# Remote file exclusion

## CTR-REMOTE-006 — Remote file exclusion

The remote store owns one optional `.exclude` sidecar per remote identity
beside its `.patch` sidecar under `$HOME/.skills-mgr/skills/.remote-patches/`.
It holds one validated pattern per line, sorted and deduplicated, and survives
refreshes for the same reason the patch does. The store's prepare step reads it
and passes the patterns to the content provider, whose shared checkout walk
skips excluded paths before reading files or charging them to the file-count
and size limits. The persisted record keeps the patterns its content was
fetched with; a record whose patterns differ from the sidecar is not fresh.

Repository installation validates the patterns, applies them to its discovery
checkout, and replaces the sidecar before calling the existing select path. If
that path fails, installation restores the previous sidecar. Uninstall removes
the sidecar together with the override and patch.

This contract supports `REQ-LAYER-002`.
