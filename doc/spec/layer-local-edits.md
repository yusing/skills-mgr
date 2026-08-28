# Layer local edits over remote skill content

## REQ-LAYER-001 — Layer local edits over remote skill content

An installed remote skill shall be editable without modifying its fetched
provider files. After the editor exits successfully, the manager shall store
the difference from the fetched `SKILL.md` as a stable local patch associated
with the remote identity under the global manager home, outside the refetchable
remote cache. The stored body shall be a readable conventional unified diff,
guarded by base-content and patched-result digests. Re-editing shall start from
the currently patched content, and uninstalling the remote skill shall remove
the patch.

`skills-mgr get` shall apply that patch before its existing Markdown
frontmatter and line-range processing. If the patch cannot be parsed or applied
after a provider refresh, `get` shall write the unmodified requested provider
content to standard output, report a compact diagnostic to standard error, and
exit nonzero.

Acceptance examples:

- Saving a remote edit creates a patch while the fetched `SKILL.md` remains
  byte-for-byte unchanged.
- The patch shows literal context, removed lines, and added lines rather than an
  encoded patch payload.
- A second edit opens the first edit's patched result.
- A provider refresh retains the patch and still applies it when `SKILL.md`
  itself is unchanged.
- Installing into an empty remote cache retains a matching patch restored in
  the global manager home.
- When two editors start from the same local layer, saving one causes the
  other's later save to fail instead of overwriting the first edit.
- An incompatible or malformed patch returns the provider body and a nonzero
  result rather than partially patched output.
- Removing the local changes removes the patch, and uninstall removes any
  remaining patch.
