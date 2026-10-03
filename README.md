# skills-mgr

One skill selection for Codex, Claude Code, and Grok, narrowed to the project
you are actually in.

`skills-mgr` finds every `SKILL.md` your machine already has, records which of
them apply to the current directory, and serves their instructions and scripts
to an agent on demand. A selection entry is an on/off switch or a condition such
as `lang go && tooling shadowtree`, so a Go project and a React project see
different skills without you touching anything. Its terminal interface also
installs skills from [skills.sh](https://skills.sh),
[SkillsMP](https://skillsmp.com), or a repository address.

## Quick Start

Requirements: Go 1.26 or newer, and Git for installing remote skills.

```sh
go install github.com/yusing/skills-mgr@latest
```

From the project whose selection you want to manage, open the interface:

```sh
skills-mgr
```

Move with `j`/`k` or the arrow keys and press Space to enable a skill. This
writes `.skills-mgr.json` in the current directory. Then see what an agent
would see:

```sh
skills-mgr list
```

```xml
<skills>
  <skill name="codebase-design" description="Shared vocabulary for designing deep modules.">
    <references>DEEPENING.md
DESIGN-IT-TWICE.md</references>
  </skill>
</skills>
```

Read a skill, or part of one of its references:

```sh
skills-mgr get codebase-design
skills-mgr get codebase-design/DEEPENING.md 1:40
```

To feed this to every session automatically, see
[Agent Integration](#agent-integration).

## Why skills-mgr?

Each agent harness discovers skills from its own directory, so a skill
installed for Claude Code is invisible to Codex. And every harness loads the
name and description of everything it discovers into every session: fifty
skills means fifty descriptions in the prompt, most of them irrelevant to the
repository you opened.

`skills-mgr` separates two questions those directories conflate:

- **Is this skill available?** Answered once per machine, by
  [discovery](#skill-discovery).
- **Does this skill apply here?** Answered per project, by a
  [selection entry](#the-selection-file) that can be a condition.

`list` advertises the enabled set, `get` prints a skill file, and `run`
executes a skill script; `get` and `run` refuse a skill that is not enabled
here. Disabling a skill never deletes it, and enabling one never copies it into
an agent's directory.

Vercel's `npx skills` is complementary: it installs skills into each agent's
directory, where installed means active. `skills-mgr` decides which installed
skills apply and serves only those, and can install from skills.sh itself.

## Managing Skills

### The Interactive Interface

The interface needs a terminal. Its tabs, selected with `←` and `→`:

| Tab | Contents |
| --- | --- |
| Installed | The project and user `.agents/skills` roots, the manager home, and installed remote skills |
| Codex | Codex-owned skills, with User, Plugin, Builtin, and System subtabs |
| Grok | `./.grok/skills`, with Plugin and Bundled subtabs |
| Claude | `./.claude/skills`, with a Plugin subtab |
| skills.sh | Browse, search, and install |
| SkillsMP | Browse, search, and install |

Only the two registry tabs use the network: the registry, then a
`git clone --depth 1` of the skill's repository.

| Key | Action |
| --- | --- |
| `←` / `→` | Change tab |
| `[` / `]` | Cycle subtabs |
| `j` / `k`, `↓` / `↑` | Move through results |
| `f` | Filter local skills, or search the active registry; Enter or Esc finishes |
| Enter or click | Expand or collapse details |
| Space | Enable or disable the selected skill |
| `i` | Edit this skill's `enabled` value in `$EDITOR`; saving it empty removes the entry |
| `e` | Edit the skill's `SKILL.md` in `$EDITOR` |
| `m` | Toggle `disable-model-invocation` for the skill |
| `a` | Move a `$HOME/.agents/skills` skill into the [manager home](#the-manager-home), or back |
| `u` | Uninstall the selected remote skill |
| `q` or Ctrl-C | Quit; during an installation, Ctrl-C cancels it first |

Editing a remote skill with `e` or `m` leaves the fetched files unchanged and
stores your change locally; see [The Remote Store](#the-remote-store). In
project mode, `u` refuses a skill configured globally; uninstall it from
`skills-mgr -g`.

Grok and Claude plugin rows follow their harness's own enabled state. Space on
a Grok Plugin or Bundled row edits `skills.disabled` in
`$HOME/.grok/config.toml`, which is global. Claude Plugin rows are
display-only.

### Installing From a Repository

```sh
skills-mgr install https://github.com/owner/repo
skills-mgr install https://github.com/owner/repo/tree/main/skills/my-skill
skills-mgr install -g owner/repo my-skill
```

The address is an HTTPS Git repository on any host, a GitHub `/tree/<ref>/<path>`
or `/blob/<ref>/<path>/SKILL.md` URL, or `owner/repo`. Encode a `/` in a branch
name as `%2F`. SSH addresses and URLs with credentials, queries, or fragments
are rejected. When the address holds several skills, name one; the error lists
the candidates.

The skill is stored with other remote skills and enabled for this project, or
globally with `-g`.

### Global and Project Layers

`skills-mgr -g` manages `$HOME/.skills-mgr/.skills-mgr.json`, the selection for
every project. Run from `$HOME`, the interface, `install`, and `sync` use it
without `-g`. A project entry in `./.skills-mgr.json` overrides the global
entry of the same name; deleting it restores inheritance.

With no entry in either layer, a project still enables skills in its own
`./.agents/skills`, and skills with `disable-model-invocation: true`. The latter
never appear in `list`, so the agent does not pick them on its own, but `get`
and `run` serve them when named.

## Agent Integration

Every harness needs two pieces: a hook that injects the inventory, and an
instruction that tells the agent how to read a skill.

### Injecting the Inventory

Run `skills-mgr list` from a hook. It detects the harness from `CLAUDECODE`,
`GROK_AGENT` or `GROK_SESSION_ID`, and `CODEX_THREAD_ID`, and omits skills that
harness already loads itself. Codex, in `~/.codex/hooks.json`:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          { "type": "command", "command": "skills-mgr list", "timeout": 5 }
        ]
      }
    ],
    "SubagentStart": [
      {
        "hooks": [
          { "type": "command", "command": "skills-mgr list", "timeout": 5 }
        ]
      }
    ]
  }
}
```

Claude Code takes the same shape in `~/.claude/settings.json`, under a
`"matcher": "*"` wrapper. Grok takes it as a JSON file in `~/.grok/hooks/`.
Register every event that starts a fresh context:

| Harness | Events |
| --- | --- |
| Codex | `SessionStart`, `SubagentStart` (Codex reruns `SessionStart` after compaction) |
| Claude Code | `SessionStart`, `SubagentStart`, `PostCompact` |
| Grok | `SessionStart`, `PostCompact` |

Pass `--claude`, `--grok`, or `--codex` only when the environment is cleared or
sets more than one marker; without exactly one harness, `list` omits nothing.

### The Instruction

Put this in `~/.codex/AGENTS.md`, `~/.claude/CLAUDE.md`, and
`~/.grok/AGENTS.md`:

```markdown
## Skills

Read a skill whose listed description matches the operation you are about to
perform, just in time and exactly once per context. Start with the most
specific owner, and add another only when it covers a separate responsibility.
Keep a loaded skill active across follow-ups.

Read a skill's instructions with `skills-mgr get <skill-name> [start:end]`, and
a listed reference with `skills-mgr get <skill-name>/<relative-path>
[start:end]`. Omit the optional 1-based inclusive range to read the whole file,
and load only the references you actually need.
Run scripts with `skills-mgr run <skill-name>/<relative/script> [args...]`.

If a skill is missing or unreadable, say so briefly and carry on.
```

### Skills Outside the Selection

The selection governs skills in the manager's stores. Skills left where a
harness looks on its own stay outside it:

| Harness | Skills it loads itself |
| --- | --- |
| Codex | Codex-owned locations and plugins |
| Claude Code | `.claude/skills` and `~/.claude/skills` |
| Grok | `.grok/skills`, `~/.grok/skills`, the shared `.agents/skills` roots, and the `.claude/skills` roots |

Grok's Claude compatibility can be turned off with
`GROK_CLAUDE_SKILLS_ENABLED=false`; `skills-mgr` does not read the equivalent
`[compat.claude]` setting.

Codex can also drop its own skills catalog so `skills-mgr` supplies it. In
`config.toml`, point `model_instructions_file` at a copy of the
[stock base instructions](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/prompts/base_instructions/default.md)
with its `## Using skills` section deleted, and set:

```toml
[skills]
include_instructions = false
```

## Command Reference

Every command uses the current working directory as the project.

| Command | Result |
| --- | --- |
| `skills-mgr [-g]` | Open the project interface, or the global one |
| `skills-mgr help` | Print usage |
| `skills-mgr info` | Show the paths `skills-mgr` uses, marking absent ones `[MISSING]` |
| `skills-mgr adopt` | Move all skills from `$HOME/.agents/skills` into the manager home |
| `skills-mgr install [-g] <address> [skill]` | Install a repository skill and enable it |
| `skills-mgr list` | Write the enabled skills as XML: name, description, and Markdown references |
| `skills-mgr get <skill>[/<path>] [start:end]` | Write a skill file, or an inclusive 1-based line range of it |
| `skills-mgr run <skill>/<script> [args...]` | Run a skill script, passing through its streams and exit status |
| `skills-mgr sync` | Download the remote skills a selection records |

`get` strips YAML frontmatter, except that the `SKILL.md` of a
`disable-model-invocation` skill keeps its `name` and `description`. If a provider update breaks a
remote skill's local edit, `get` prints the unedited body, reports the error,
and exits nonzero.

`run` executes the script from the skill directory. Non-executable `.py` files
run under `python3`, and JavaScript or TypeScript files under `node` or `bun`.

## Skill Discovery

A skill is a directory with a `SKILL.md` whose frontmatter has a `name`
(letters, digits, `.`, `_`, `-`, `:`, up to 80 characters) and a
`description`. Discovery reads these roots in order; the first to declare a
name owns it:

| Priority | Root | Source |
| --- | --- | --- |
| 1 | `./.agents/skills` | `project` |
| 2 | `$HOME/.agents/skills` | `user` |
| 3 | `$HOME/.skills-mgr/skills` | `managed` |
| 4 | `./.claude/skills` | `claude` |
| 5 | `./.grok/skills` | `grok` |
| 6 | `./.codex/skills` | `codex` |
| 7 | `$CODEX_HOME/skills` (default `$HOME/.codex/skills`) | `codex` |
| 8 | `/etc/codex/skills` | `admin` |
| 9 | `skills` directories in `$CODEX_HOME/plugins/cache` | `plugin` |
| 10 | The remote store | The provider name |

A `.system` directory in a Codex root is reported as `bundled`. Invalid entries
are skipped silently. Under a harness scope, roots 4 to 9 that belong to another
harness are dropped.

## The Selection File

`.skills-mgr.json` is JSON at schema revision `3`; older revisions are upgraded
on the next write. See [`skills-mgr.schema.json`](skills-mgr.schema.json).

```json
{
  "schema_revision": 3,
  "skills": {
    "writing-readme": { "enabled": true },
    "unused-skill": { "enabled": false },
    "golang-best-practices": { "enabled": "lang go" }
  }
}
```

Entries for skills that no longer exist are kept and ignored. Commit the file
when a repository should share its selection.

### Conditional Expressions

> An `enabled` string runs as Bash, with your permissions, whenever the skill's
> state is checked. Review committed `.skills-mgr.json` changes before running
> `skills-mgr` in a repository you do not trust.

The string is evaluated by [`mvdan.cc/sh`](https://mvdan.cc/sh) in the project
directory. Exit status `0` enables the skill, `1` disables it, and anything
else is an error, which makes `list` fail. External commands work, such as
`command -v herdr`. Three builtins inspect the project without starting a
process:

```sh
lang ts && (tooling pnpm || tooling bun)
has_dependency tauri '>=2 && <3'
```

#### `has_dependency <name> ['<op><version>']`

Tests for a dependency in any `go.mod`, `Cargo.toml`, or `package.json`.
Operators are `>=`, `==`, `<=`, and `<`, combinable with `&&` and `||`.
Comparisons use the precision you give, so `==2` matches any `2.x`. Version
ranges in manifests are read by their numeric bounds, so exact declarations are
the most predictable. Indirect Go requirements and npm `peerDependencies` do
not count.

#### `lang <language>`

Tests for a package marker or file extension. Extensions match
case-insensitively, except `.C`, which means C++.

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
| `r` | `.r`, `.rmd`, `.rproj` |
| `ruby` | `Gemfile`, `.rb` |
| `swift` | `Package.swift`, `.swift` |
| `perl` | `cpanfile`, `.pl`, `.pm` |
| `assembly` / `asm` | `.asm`, `.s` |
| `shell` / `sh` | `.sh` |
| `bash` | `.bash` |
| `postgres` | `postgresql.conf`, `pg_hba.conf`, `pg_ident.conf`, `.psql` |
| `sql` | `.sql` |
| `yaml` | `.yaml`, `.yml` |
| `json` | `.json` |
| `toml` | `.toml` |
| `ini` | `.ini` |

#### `tooling <name>`

Tests for a tool's lockfile, configuration, or build entrypoint in the
project, not whether the tool is installed.

| Tool | Project evidence |
| --- | --- |
| `bun` | `bun.lock`, `bun.lockb`, `bunfig.toml` |
| `yarn` | `yarn.lock`, `.yarnrc`, `.yarnrc.yml`, `.yarnrc.yaml` |
| `deno` | `deno.json`, `deno.jsonc`, `deno.lock` |
| `npm` | `package-lock.json`, `npm-shrinkwrap.json` |
| `pnpm` | `pnpm-lock.yaml`, `pnpm-workspace.yaml` |
| `maven` | `pom.xml`, `mvnw`, `mvnw.cmd` |
| `composer` | `composer.json`, `composer.lock` |
| `cmake` | `CMakeLists.txt`, `CMakePresets.json`, `CMakeUserPresets.json` |
| `make` | `Makefile`, `makefile`, `GNUmakefile` |
| `just` | `justfile`, `Justfile`, `.justfile` |
| `shadowtree` | `.shadowtree.toml` |
| `taskfile` | `Taskfile.yml`, `Taskfile.yaml`, `taskfile.yml`, `taskfile.yaml` |
| `bazel` | `.bazelrc`, `MODULE.bazel`, `WORKSPACE`, `WORKSPACE.bazel`, `BUILD`, `BUILD.bazel` |
| `docker` | `Dockerfile`, `Dockerfile.*`, `docker-bake.hcl`, `docker-bake.json`, or Docker Compose evidence |
| `docker-compose` | `compose.yml`, `compose.yaml`, `docker-compose.yml`, `docker-compose.yaml` |
| `kubernetes` / `k8s` | `kustomization.yml`, `kustomization.yaml`, `Kustomization`, `Chart.yaml`, `skaffold.yml`, `skaffold.yaml` |
| `pip` | `pip.conf`, `pip.ini`, or a `requirements*.txt` file |
| `uv` | `uv.lock`, `uv.toml` |

#### What the Scan Sees

The builtins share one project scan per command. It honors Git ignore rules
(tracked files always count) and skips `.git`, `node_modules`, and `target`.
From `$HOME`, only home's direct entries count, and a directory its parent
repository ignores produces no evidence.

## Where Skill Content Lives

No harness scans the manager's two stores, so the selection alone decides
whether their skills reach the agent.

### The Manager Home

Skills you write belong in `$HOME/.skills-mgr/skills`; move one there with `a`.
Left in `$HOME/.agents/skills`, a skill loads into every Grok session whatever
its entry says, and is missing from Claude Code entirely.

### The Remote Store

Installed remote skills live in the user cache, under
`skills-mgr/remote-skills`, because `sync` can refetch them. Content is never
executed at install time. A skill over 1024 files or 16 MiB is rejected. Set
`SKILLSMP_API_KEY` to authenticate SkillsMP requests.

Local edits to a remote skill are stored in
`$HOME/.skills-mgr/skills/.remote-patches/`, outside the cache, so they can be
tracked with the manager home. They survive refreshes and are removed on
uninstall.

### Placeholders

Enabling a skill from either store writes a stub `SKILL.md` under
`.agents/skills/` and `.claude/skills/` (under `$HOME` in global mode), so the
harness offers its name as a slash command. The stub sets
`disable-model-invocation: true`, so the model never sees it; `list` remains
the only model-facing view. Disabling removes the stub and keeps the content.

### Reproducing a Selection Elsewhere

A committed `.skills-mgr.json` records enough to refetch each remote skill. On
another machine:

```sh
skills-mgr sync
```

`sync` downloads every remote skill in the global selection, and the project's
own remote skills that are enabled. Downloading does not enable anything. A
failure names the skill, exits nonzero, and leaves selection files unchanged.
Nothing else downloads a skill it has never seen.

If cached content disappears, `sync` refetches it; meanwhile the Installed tab
shows a `[content missing]` row you can uninstall.

## Background Refresh

Any command except `info` may start a detached background runner, at most once
every five minutes. It refreshes skills.sh metadata, updates
installed remote skills older than three hours, and repairs manager-home
placeholders. It never downloads new skills. Failures go to `refresh.log` in
the user cache `skills-mgr` directory.

## Development

The repository uses [Shadowtree](https://github.com/yusing/shadowtree) recipes.
Design notes are in [`doc/`](doc/).

| Command | Result |
| --- | --- |
| `shadowtree build` | Compile all packages |
| `shadowtree test` | Run the test suite |
| `shadowtree check` | Run `go vet`, then the tests |
| `shadowtree test-race` | Run tests with the race detector |
| `shadowtree lint` | Run `golangci-lint` |
| `shadowtree fmt` | Format the source |
| `shadowtree tidy` | Tidy `go.mod` and `go.sum` |
| `shadowtree install` | Install the checkout |

## License

MIT. See [`LICENSE`](LICENSE).
