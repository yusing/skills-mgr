# skills-mgr

Discover, enable, read, and run agent skills from one terminal interface.

`skills-mgr` finds local `SKILL.md` packages, records which skills are enabled
for the current directory, and exposes their instructions and scripts to agent
workflows. Its interactive interface can also install skills from
[skills.sh](https://skills.sh) and [SkillsMP](https://skillsmp.com).

## Quick Start

Requirements:

- Go 1.26 or newer
- Git, when installing a remote skill

Install the command:

```sh
go install github.com/yusing/skills-mgr@latest
```

From the project whose skill selection you want to manage, open the interactive
interface:

```sh
skills-mgr
```

Move with `j`/`k` or the arrow keys and press Space to enable a skill. A local
selection writes `.skills-mgr.json` in the current directory. Then print the
enabled skills as Markdown:

```sh
skills-mgr list
```

## Interactive Interface

The interface has three tabs:

| Tab | Purpose | Network behavior |
| --- | --- | --- |
| Local | Browse discovered skills and enable them for the current project | None |
| skills.sh | Browse topics, search the skills.sh registry, and install skills | Contacts skills.sh and clones the selected skill's GitHub repository |
| SkillsMP | Browse or search SkillsMP and install skills | Contacts SkillsMP and clones the selected skill's GitHub repository |

Remote skill content and registry responses are stored below the user cache
directory in `skills-mgr/`. Enabling a project remote creates autocomplete
placeholders under `.agents/skills/` and `.claude/skills/`. Global mode creates
the placeholders under `$HOME`. Disabling a remote removes its managed
placeholders but does not immediately remove its cached content.

The SkillsMP client sends `SKILLSMP_API_KEY` as a bearer token when that
environment variable is set. Requests are unauthenticated when it is unset.

| Key | Action |
| --- | --- |
| `←` / `→` | Change tab |
| `j` / `k`, `↓` / `↑` | Move through results |
| `f` | Filter local skills or search the active registry |
| Enter or click | Expand or collapse details |
| Space | Enable or disable the selected skill |
| `e` | Open an editable local skill in `$EDITOR` |
| `q` or Ctrl-C | Quit |

### Global selections

Run the interface with `-g` to manage the global selection:

```sh
skills-mgr -g
```

This mode writes `$HOME/.skills-mgr.json` and remote autocomplete placeholders
under `$HOME/.agents/skills/` and `$HOME/.claude/skills/`. In normal project
mode, entries in the current directory's `.skills-mgr.json` override global
entries with the same skill name.

## Command Reference

All project commands use the current working directory as the project.
`get` and `run` reject skills that are not enabled.

| Command | Result |
| --- | --- |
| `skills-mgr` | Open the project selection interface |
| `skills-mgr -g` | Open the global selection interface |
| `skills-mgr list` | Write enabled skill names, descriptions, and reference-file trees to stdout as Markdown |
| `skills-mgr get <skill>` | Write the body of the skill's `SKILL.md` to stdout |
| `skills-mgr get <skill>/<path>` | Write a file from the enabled skill to stdout |
| `skills-mgr get <skill>/<path> <start>:<end>` | Write an inclusive, 1-based line range |
| `skills-mgr run <skill>/<script> [args...]` | Run a script from the enabled skill and pass through its standard streams and exit status |
| `skills-mgr daemon` | Refresh registry metadata and stale installed remote skills until interrupted |

Markdown returned by `get` omits YAML frontmatter. For a line-range request,
line numbers apply to that frontmatter-free content.

`run` sets the skill directory as the script's working directory. Executable
files run directly, non-executable `.py` files use `python3`, and
non-executable JavaScript or TypeScript files use the first available runtime
from `node` and `bun`.

Examples:

```sh
# Read an enabled skill's main instructions.
skills-mgr get writing-readme

# Read part of an enabled skill's reference file.
skills-mgr get deliver-vertical-slice/references/validation-scenarios.md 10:30
```

## Skill Discovery

Each skill is a directory containing a `SKILL.md` with YAML frontmatter that
provides a valid `name` and non-empty `description`.

Discovery checks these sources in order:

| Priority | Source |
| --- | --- |
| 1 | `./.agents/skills/<skill>/SKILL.md` |
| 2 | `$HOME/.agents/skills/<skill>/SKILL.md` |
| 3 | `$CODEX_HOME/skills/<skill>/SKILL.md`, or `$HOME/.codex/skills/<skill>/SKILL.md` when `CODEX_HOME` is unset |
| 4 | `/etc/codex/skills/<skill>/SKILL.md` |
| 5 | Skill directories in the Codex plugin cache |
| 6 | Skills installed from a remote registry |

When multiple sources declare the same skill name, the first discovered source
wins. Missing, unreadable, or invalid skill entries are skipped.

## Agent integration

Add this to your global `AGENTS.md`

```markdown
## Using skills

Run `skills-mgr list` from the current project directory to discover the enabled
skills, their descriptions, and their available reference files.

Use every explicitly requested enabled skill, whose descriptions
match the current work, or those required by the skill you have used.
Before applying a selected skill, read its complete instructions
with `skills-mgr get <skill-name>`. Read a listed reference with
`skills-mgr get <skill-name>/<relative-path>`. The optional final `start:end` argument
selects a 1-based inclusive line range; omit it when the complete file is required.

Invoke a script provided by an enabled skill with
`skills-mgr run <skill-name>/<relative/script> [args...]`. Arguments are passed
directly to the script. Executable files honor their shebang; non-executable Python
files use `python3`, while non-executable `.js`, `.mjs`, `.cjs`, `.ts`, `.mts`, and
`.cts` files use the cached `node`-then-`bun` runtime fallback.

Load skills just-in-time, instaed of loading everything and forget later.

Keep unchanged skills active across follow-ups, and do not
load unrelated references. If a named skill is unavailable or cannot be read, say so
briefly and continue with the best fallback. User instructions and higher-priority
instructions override skill guidance.
```

For `codex`, turn off default skills instructions by:

- overriding default base prompt: replace `## Using skills` section instead of adding above to `AGENTS.md`
- disable skills instructions

```toml
model_instructions_file = "/path/to/your/base_instructions.md"

[skills]
include_instructions = false
```

Default base prompts could be found in:

- <https://github.com/openai/codex/blob/main/codex-rs/protocol/src/prompts/base_instructions/default.md>
- <https://github.com/asgeirtj/system_prompts_leaks/blob/main/OpenAI/Codex/gpt-5.6.md>

## Selection State

The selection file is strict JSON with schema revision `1`:

```json
{
  "schema_revision": 1,
  "skills": {
    "writing-readme": true,
    "unused-skill": false
  }
}
```

Use the interactive interface to update this file. `true` enables a discovered
skill; `false` explicitly disables it. Project values override inherited global
values.

Commit a project's `.skills-mgr.json` when the repository should share a
selection. Every enabled name must still be discoverable on each machine or
`skills-mgr list` will fail. Update coordination locks are kept in the user
cache rather than the project directory.

## Remote Refresh Daemon

`skills-mgr daemon` refreshes registry metadata immediately, then checks every
five minutes. Installed remote content is refreshed after it becomes stale.
Failures are written to stderr and do not stop later refresh cycles.

The included `skills-mgr.service` is a systemd user-unit template. Verify that
its `ExecStart` path matches the installed binary before installing it.

## Development

Clone the repository and use its [Shadowtree](https://github.com/yusing/shadowtree)
recipes:

```sh
git clone https://github.com/yusing/skills-mgr.git
cd skills-mgr
shadowtree check
```

Common development workflows:

| Workflow | Command | Result |
| --- | --- | --- |
| Build | `shadowtree build` | Compile all Go packages in a sandbox |
| Test | `shadowtree test` | Run the Go test suite |
| Vet and test | `shadowtree check` | Run `go vet` followed by the test suite |
| Race detection | `shadowtree test-race` | Run tests with the race detector |
| Lint | `shadowtree lint` | Run `golangci-lint` |
| Format | `shadowtree fmt` | Format Go source in the current checkout |
| Install checkout | `shadowtree install` | Install the main package with stripped debug information |
