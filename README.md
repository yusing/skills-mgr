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

The interface has six tabs:

| Tab | Purpose | Network behavior |
| --- | --- | --- |
| Installed | Browse user and project skills and enable them for the current project | None |
| Codex | Browse Codex-native skills. `[` / `]` switch the User, Plugin, Builtin, and System source lists | None |
| Grok | Browse Grok-native skills | None |
| Claude | Browse Claude-native skills | None |
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
| `←` / `→` | Change tab. From Installed, right opens Codex |
| `[` / `]` | On the Codex tab, cycle source subtabs (User, Plugin, Builtin, System) |
| `j` / `k`, `↓` / `↑` | Move through results |
| `f` | Filter local skills or search the active registry |
| Enter or click | Expand or collapse details |
| Space | Enable or disable the selected skill |
| `e` | Open an editable local skill in `$EDITOR` |
| `i` | Edit the selected skill's current-layer `enabled` JSON value in `$EDITOR` |
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
| `skills-mgr daemon refresh` | Ask the running daemon to refresh the skills.sh registry cache now |
| `skills-mgr daemon sync` | Ask the running daemon to update stale persisted remote skills now |

Markdown returned by `get` omits YAML frontmatter. For a line-range request,
line numbers apply to that frontmatter-free content.

`list`, `get`, and `run` accept `--claude`, `--grok`, and `--codex` to hide
skills the named agent already loads on its own. When those flags are omitted,
the command infers a single calling agent from `CLAUDECODE`, `GROK_AGENT` or
`GROK_SESSION_ID`, or `CODEX_THREAD_ID`. If none of those session markers is
set, or more than one is set, the command stays unscoped.

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

The selection file is strict JSON with schema revision `3`. An `enabled`
value is either a Boolean or a Bash expression. Project expressions execute
with the user's environment and filesystem permissions when the skill is
checked, so review committed `.skills-mgr.json` changes before running
`skills-mgr` in an untrusted repository.

```json
{
  "schema_revision": 3,
  "skills": {
    "writing-readme": {
      "enabled": true
    },
    "unused-skill": {
      "enabled": false
    },
    "go-review": {
      "enabled": "[ -f go.mod ]"
    }
  }
}
```

Use the interactive interface to update this file. `true` enables a discovered
skill; `false` explicitly disables it. A string is parsed with Bash syntax by
`mvdan.cc/sh` and evaluated from the current project directory. Final status
`0` enables the skill, status `1` disables it, and status `2` through `255`
is an error. External commands in an expression are still started normally.
Project values override inherited global values. Removing a project value
resumes global inheritance.

Expressions can use `has_dependency <name>` to test for a dependency declaration,
or `has_dependency <name> '<operator><version>'` to also test its declared version,
in `go.mod`, `Cargo.toml`, or `package.json` files anywhere in the project tree.
The supported comparison operators are `>=`, `==`, `<=`, and `<`. A quoted version
argument can combine comparisons with `&&` and `||`; `&&` is evaluated before
`||`, and every comparison in an `&&` group must match the same declaration.
Comparisons use the precision supplied, so `==2` matches any declared version in
major version `2`, while `==2.1` also compares the minor version.

The builtin ignores indirect Go requirements and recognizes Cargo dependency
versions plus package dependencies, development dependencies, and optional
dependencies. Cargo `[workspace.dependencies]` catalog entries count even when no
workspace member inherits them. A dependency without a declared version, such as
a Cargo path dependency, matches only the name-only form.

Manifest version checks compare the numeric boundaries written in a declaration;
they do not fully interpret npm or Cargo range semantics. Exact numeric declarations
produce the most predictable result; caret, tilde, compound, and alternative
manifest ranges are evaluated only by their written numeric boundaries.

Use `lang <language>` to test whether the project contains any matching package
marker or file extension. Mixed-language projects satisfy every language found.
Supported names are `go`, `rust`, `node`, `typescript` (`ts`), `tsx`,
`javascript` (`js`), `jsx`, `html`, `css`, `python`, `c`, `c++`, `c#`, `java`,
`lua`, `vb`, `php`, `r`, `ruby`, `swift`, `perl`, `assembly` (`asm`), `shell`
(`sh`), `bash`, `postgres`, `sql`, `yaml`, `json`, `toml`, and `ini`. Names and
aliases are lowercase.

Detection uses this evidence:

| Language | Package markers and file extensions |
| --- | --- |
| `go` | `go.mod`, `.go` |
| `rust` | `Cargo.toml`, `.rs` |
| `node` | `package.json` |
| `typescript` / `ts` | `tsconfig.json`, `.ts`, `.tsx`, `.mts`, `.cts` |
| `tsx` | `.tsx` |
| `javascript` / `js` | `.js`, `.jsx`, `.mjs`, `.cjs` |
| `jsx` | `.jsx` |
| `html` | `.html`, `.htm` |
| `css` | `.css` |
| `python` | `pyproject.toml`, `requirements.txt`, `Pipfile`, `.py`, `.pyw` |
| `c` | `.c` |
| `c++` | `.C`, `.cc`, `.cpp`, `.cxx`, `.c++`, `.hh`, `.hpp`, `.hxx` |
| `c#` | `.cs`, `.csproj` |
| `java` | `.java` |
| `lua` | `.lua` |
| `vb` | `.vb`, `.vbproj` |
| `php` | `composer.json`, `.php` |
| `r` | `.r`, `.rmd`, `.rproj`, matched case-insensitively |
| `ruby` | `Gemfile`, `.rb` |
| `swift` | `Package.swift`, `.swift` |
| `perl` | `cpanfile`, `.pl`, `.pm` |
| `assembly` / `asm` | `.asm`, `.s`, matched case-insensitively |
| `shell` / `sh` | `.sh` |
| `bash` | `.bash` |
| `postgres` | `postgresql.conf`, `pg_hba.conf`, `pg_ident.conf`, `.psql` |
| `sql` | `.sql` |
| `yaml` | `.yaml`, `.yml` |
| `json` | `.json` |
| `toml` | `.toml` |
| `ini` | `.ini` |

An evidence file can detect more than one language. For example, `Cargo.toml`
detects both `rust` and `toml`, and `package.json` detects both `node` and
`json`. Each enabled expression uses one cached, resumable project walk. A
predicate stops the walk as soon as its evidence is found; a later predicate in
the same expression reuses evidence already seen and resumes the remaining
directories. The walk includes Git-ignored files and skips `.git`,
`node_modules`, and `target` directories.

For example, this expression enables a skill in a project containing Go or
TypeScript evidence:

```sh
lang go || lang ts
```

For example, this global override enables the `tauri-v2` skill when the project
declares or catalogs Tauri, its JavaScript API, or its CLI at a major version `2`:

```json
{
  "schema_revision": 3,
  "skills": {
    "tauri-v2": {
      "enabled": "has_dependency tauri '>=2 && <3' || has_dependency '@tauri-apps/api' '>=2 && <3' || has_dependency '@tauri-apps/cli' '>=2 && <3'"
    }
  }
}
```

Press `i` in the TUI to edit one value without exposing the rest of the
selection file. Enter `true`, `false`, a bare Bash expression, or a JSON string;
save an empty file to remove the current-layer value. A conditional remote entry
does not create managed autocomplete placeholders in its own layer, because
those placeholders would bypass a false condition. A project override cannot
hide a placeholder created by an inherited global `true` entry from native
harness discovery; `skills-mgr list`, `get`, and `run` still honor the project
override.

Commit a project's `.skills-mgr.json` when the repository should share a
selection. Configured names that are not discovered are retained and ignored.
Update coordination locks are kept in the user cache rather than the project
directory. Schema revisions `1` and `2` remain readable and are upgraded on
the next selection write.

## Remote Refresh Daemon

`skills-mgr daemon` listens on `$XDG_RUNTIME_DIR/skills-mgr.sock`, or
`skills-mgr.sock` under the user cache `skills-mgr` directory when
`XDG_RUNTIME_DIR` is unset. It refreshes skills.sh registry metadata
immediately, then checks every five minutes. Installed remote content is
refreshed after it becomes stale.

`skills-mgr daemon refresh` asks that running process to refresh the registry
cache. `skills-mgr daemon sync` asks it to update stale persisted remote
skills. Both wait until the work finishes. They fail if the daemon is not
running and do not download previously unknown remote identities.

The daemon writes structured text logs to stderr for start, stop, inbound
commands, cache refresh, and each remote-skill update. Failures are logged
and do not stop later refresh cycles.

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
