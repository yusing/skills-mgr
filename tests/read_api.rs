use std::{fs, path::PathBuf};

use serde_json::json;
use skills_mgr::{Error, Harness, SkillsManager};
use tempfile::{TempDir, tempdir};

struct Fixture {
    temp: TempDir,
    manager: SkillsManager,
}

impl Fixture {
    fn condition(&self, expr: &str) {
        self.write(
            "home/.skills-mgr/.skills-mgr.json",
            json!({
                "schema_revision":3, "skills":{"managed":{"enabled":expr}}
            })
            .to_string(),
        );
    }

    fn assert_condition(&self, expr: &str, want: bool) {
        self.condition(expr);
        assert_eq!(
            self.names().contains(&"managed".to_string()),
            want,
            "list: {expr}"
        );
        match self.manager.get("managed") {
            Ok(body) if want => assert_eq!(body, b"Managed\n"),
            Err(Error::NotEnabled(_)) if !want => {}
            other => panic!("get: {expr}: {other:?}"),
        }
    }

    fn new() -> Self {
        let temp = tempdir().unwrap();
        let manager = SkillsManager::new(
            temp.path().join("project"),
            temp.path().join("home"),
            temp.path().join("cache"),
        );
        Self { temp, manager }
    }

    fn write(&self, path: &str, bytes: impl AsRef<[u8]>) -> PathBuf {
        let path = self.temp.path().join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(&path, bytes).unwrap();
        path
    }

    fn skill(&self, path: &str, name: &str, body: &str) -> PathBuf {
        self.write(
            &format!("{path}/SKILL.md"),
            format!("---\nname: {name}\ndescription: Useful {name} skill\n---\n{body}"),
        )
    }

    fn names(&self) -> Vec<String> {
        self.manager
            .list()
            .unwrap()
            .into_iter()
            .map(|skill| skill.name)
            .collect()
    }
}

#[test]
fn reports_adoption_without_creating_or_parsing_selection_files() {
    let f = Fixture::new();
    f.skill("project/.agents/skills/local", "local", "Local\n");
    assert!(!f.manager.is_enabled().unwrap());
    assert_eq!(f.names(), ["local"]);

    let project_lock = f.write(
        "project/.skills-mgr.json",
        r#"{"schema_revision":3,"skills":{}}"#,
    );
    assert!(f.manager.is_enabled().unwrap());
    fs::remove_file(project_lock).unwrap();

    let global_lock = f.write("home/.skills-mgr/.skills-mgr.json", "invalid JSON");
    assert!(f.manager.is_enabled().unwrap());
    assert!(matches!(f.manager.list(), Err(Error::InvalidData { .. })));
    fs::remove_file(&global_lock).unwrap();

    let legacy = f.write(
        "home/.skills-mgr.json",
        r#"{"schema_revision":1,"skills":{"managed":true}}"#,
    );
    f.skill(
        "home/.skills-mgr/skills/actual-dir",
        "managed",
        "Inherited\n",
    );
    assert!(f.manager.is_enabled().unwrap());
    assert_eq!(f.manager.get("managed").unwrap(), b"Inherited\n");
    assert!(legacy.is_file());
    assert!(!global_lock.exists());
}

#[test]
fn shared_resolution_honors_precedence_selection_and_invocation_visibility() {
    let f = Fixture::new();
    f.skill("project/.agents/skills/local-dir", "local", "Project\n");
    f.skill("home/.agents/skills/user-dir", "local", "User\n");
    f.skill("home/.skills-mgr/skills/picked", "picked", "Managed\n");
    f.skill("project/.agents/skills/disabled", "disabled", "Disabled\n");
    f.write("project/.agents/skills/hidden/SKILL.md",
        "---\nname: hidden\ndescription: A hidden skill\nprivate: excluded\ndisable-model-invocation: true\n---\nHidden\n");
    f.write(
        "project/.agents/skills/bad/SKILL.md",
        "---\nname: bad\n---\nMissing description",
    );
    f.write(
        "home/.skills-mgr/.skills-mgr.json",
        r#"{"schema_revision":2,"skills":{"local":{"enabled":true},"picked":{"enabled":true}}}"#,
    );
    f.write("project/.skills-mgr.json",
        r#"{"schema_revision":3,"skills":{"disabled":{"enabled":false},"picked":{"enabled":false}}}"#);

    assert_eq!(f.names(), ["local"]);
    assert_eq!(f.manager.get("local").unwrap(), b"Project\n");
    assert!(matches!(
        f.manager.get("disabled"),
        Err(Error::NotEnabled(_))
    ));
    assert!(matches!(f.manager.get("picked"), Err(Error::NotEnabled(_))));
    assert!(matches!(f.manager.get("bad"), Err(Error::NotFound(_))));
    let hidden = String::from_utf8(f.manager.get("hidden").unwrap()).unwrap();
    assert_eq!(
        f.manager.get("hidden/./SKILL.md").unwrap(),
        hidden.as_bytes()
    );
    assert!(hidden.starts_with("---\n"));
    assert!(hidden.contains("name: hidden\n"));
    assert!(hidden.contains("description: A hidden skill\n"));
    assert!(hidden.ends_with("---\nHidden\n"));
    assert!(!hidden.contains("excluded"));
    assert!(!hidden.contains("disable-model-invocation"));
}

#[test]
fn reads_tagged_booleans_written_by_the_go_cli() {
    let f = Fixture::new();
    for (name, value) in [
        ("upper-yes", "YES"),
        ("numeric-one", "1"),
        ("short-true", "t"),
        ("lower-no", "no"),
        ("numeric-zero", "0"),
        ("short-false", "F"),
    ] {
        f.write(
            &format!("project/.agents/skills/{name}/SKILL.md"),
            format!("---\nname: {name}\ndescription: Example\ndisable-model-invocation: !!bool {value}\n---\nBody\n"),
        );
    }
    assert_eq!(f.names(), ["lower-no", "numeric-zero", "short-false"]);
    assert!(
        f.manager
            .get("short-true")
            .unwrap()
            .ends_with(b"---\nBody\n")
    );
    assert_eq!(f.manager.get("numeric-zero").unwrap(), b"Body\n");
}

#[test]
fn lists_reference_paths_and_reads_served_lines_and_binary_files() {
    let f = Fixture::new();
    let root = "project/.agents/skills/example";
    f.write(&format!("{root}/SKILL.md"),
        "\u{feff}---\r\nname: example\r\ndescription: >-\r\n  An example\r\n  skill\r\n---\r\none\r\ntwo\r\nthree");
    f.write(&format!("{root}/Guide.md"), b"A plain reference\n");
    f.write(
        &format!("{root}/references/details/Some Notes.md"),
        b"---\nignored: true\n---\nfirst\nsecond\n",
    );
    f.write(
        &format!("{root}/references/_hidden/internal.md"),
        "internal",
    );
    f.write(&format!("{root}/other/unlisted.md"), "unlisted");
    f.write(&format!("{root}/scripts/data.bin"), [0, 255, 62]);
    f.write(
        &format!("{root}/references/.broken.md"),
        "---\ntruncated: true\n",
    );
    let skills = f.manager.list().unwrap();
    assert_eq!(skills[0].description, "An example skill");
    assert_eq!(
        skills[0].references,
        ["Guide.md", "references/details/Some Notes.md"]
    );
    assert_eq!(
        f.manager.get_lines("example", 2..=50).unwrap(),
        b"two\r\nthree"
    );
    assert_eq!(
        f.manager.get("example/Guide.md").unwrap(),
        b"A plain reference\n"
    );
    assert_eq!(
        f.manager
            .get("example/references/details/Some Notes.md")
            .unwrap(),
        b"first\nsecond\n"
    );
    assert_eq!(
        f.manager.get("example/scripts/data.bin").unwrap(),
        [0, 255, 62]
    );
    assert!(matches!(
        f.manager.get_lines("example", 0..=1),
        Err(Error::InvalidLineRange)
    ));
    assert!(matches!(
        f.manager.get_lines("example", 4..=5),
        Err(Error::LineOutOfBounds(4))
    ));
    assert!(matches!(
        f.manager.get("example/../SKILL.md"),
        Err(Error::InvalidTarget(_))
    ));
    assert!(matches!(
        f.manager.get("example/references/.broken.md"),
        Err(Error::InvalidData { .. })
    ));
}

#[test]
fn lists_xml_with_escaped_metadata_and_grouped_references() {
    let f = Fixture::new();
    let root = "home/.skills-mgr/skills/example";
    f.write(
        &format!("{root}/SKILL.md"),
        "---\nname: example\ndescription: \"A & B < C > D \\\"quoted\\\" 'single'\"\n---\nBody\n",
    );
    f.write(&format!("{root}/Guide.md"), "Guide\n");
    f.write(&format!("{root}/references/team notes/One.md"), "One\n");
    f.write(
        &format!("{root}/references/team notes/Two & More.md"),
        "Two\n",
    );
    assert_eq!(f.manager.list_xml(&[]).unwrap(), "<skills></skills>\n");
    f.write(
        "project/.skills-mgr.json",
        r#"{"schema_revision":3,"skills":{"example":{"enabled":true}}}"#,
    );
    assert_eq!(
        f.manager.list_xml(&[]).unwrap(),
        concat!(
            "<skills>\n",
            "  <skill name=\"example\" description=\"A &amp; B &lt; C &gt; D &#34;quoted&#34; &#39;single&#39;\">\n",
            "    <references>Guide.md 'references/team notes'/{One,'Two &amp; More'}.md</references>\n",
            "  </skill>\n",
            "</skills>\n",
        )
    );
}

#[cfg(unix)]
#[test]
fn symlinked_skill_roots_work_but_files_cannot_escape_the_skill() {
    use std::os::unix::fs::symlink;

    let f = Fixture::new();
    let manifest = f.skill("elsewhere/skill", "example", "Local");
    let root = manifest.parent().unwrap();
    let link = f.temp.path().join("project/.agents/skills/link");
    fs::create_dir_all(link.parent().unwrap()).unwrap();
    symlink(root, link).unwrap();
    let outside = f.write("elsewhere/outside.md", "Outside skill root\n");
    symlink(&outside, root.join("outside.md")).unwrap();
    symlink("SKILL.md", root.join("inside.md")).unwrap();
    assert_eq!(f.manager.list().unwrap()[0].references, ["inside.md"]);
    assert_eq!(f.manager.get("example/inside.md").unwrap(), b"Local");
    assert!(matches!(
        f.manager.get("example/outside.md"),
        Err(Error::Io { .. })
    ));
}

#[test]
fn native_conditions_use_current_project_evidence_and_shell_boolean_order() {
    let f = Fixture::new();
    f.skill("home/.skills-mgr/skills/managed", "managed", "Managed\n");
    f.write("project/src/index.TSX", "");
    f.write("project/src/foo.C", "");
    f.write("project/config/Chart.yaml", "");
    f.write("project/compose.yml", "");
    f.write("project/src/.gitignore", "*.py\n");
    f.write("project/src/generated.py", "");
    f.write("project/node_modules/leak.go", "");
    f.write("project/target/leak.rs", "");
    f.write("project/package.json", "not json");
    for expr in [
        "lang ts && lang tsx && tooling k8s",
        "lang 'c++' && ! lang c",
        "tooling docker && tooling docker-compose",
        "! (lang python || lang go || lang rust)",
        "lang js || (lang ts && ! tooling npm)",
        "lang ts || has_dependency missing",
    ] {
        f.assert_condition(expr, true);
    }
    f.assert_condition("lang ts || lang js && tooling pnpm", false);
    f.assert_condition(r#"[ "$PWD" == "$HOME" ] || lang js"#, false);
    f.assert_condition("lang ts | lang js", false);
    f.assert_condition("! lang ts | lang js", true);
    f.write("project/pnpm-lock.yaml", "");
    f.assert_condition("lang ts || lang js && tooling pnpm", true);
    f.condition("has_dependency missing");
    assert!(
        matches!(f.manager.get("managed"),Err(Error::Condition {skill,source}) if skill=="managed" && source.to_string().contains("package.json"))
    );
    for expr in ["tooling unknown", "lang", "lang unknown", "has_dependency"] {
        f.condition(expr);
        assert!(matches!(f.manager.list(),Err(Error::Condition {skill,..}) if skill=="managed"));
    }
    for expr in [
        "lang $(touch unexpected)",
        "lang ts; touch unexpected",
        "lang *",
        "lang \"$NAME\"",
    ] {
        f.condition(expr);
        assert!(matches!(
            f.manager.list(),
            Err(Error::UnsupportedCondition(_))
        ));
        assert!(matches!(
            f.manager.get("managed"),
            Err(Error::UnsupportedCondition(_))
        ));
    }
    assert!(!f.temp.path().join("project/unexpected").exists());
}

#[cfg(unix)]
#[test]
fn xml_evaluates_conditions_from_the_project_without_a_shell_wrapper() {
    use std::os::unix::fs::PermissionsExt;

    let f = Fixture::new();
    f.skill("home/.skills-mgr/skills/managed", "managed", "Managed\n");
    f.write(
        "project/Cargo.toml",
        "[package]\nname = 'fixture'\nversion = '0.1.0'\n",
    );
    f.write("project/sub/nested", "");
    f.write("home/config/witness", "");
    let probe = f.write(
        "project/probe",
        "#!/bin/sh\nprintf '%s\\n' \"$PWD\" > invoked\nprintf 'main\\n\\n'\nexit \"$1\"\n",
    );
    fs::set_permissions(&probe, fs::Permissions::from_mode(0o755)).unwrap();
    fs::copy(probe, f.temp.path().join("project/sub/probe")).unwrap();
    let invoked = f.temp.path().join("project/invoked");
    let xml = |expr: &str| {
        f.condition(expr);
        f.manager.list_xml(&[Harness::Codex])
    };
    for (expr, visible) in [
        (r#"[[ -f Cargo.toml && $HOME != "$PWD" ]]"#, true),
        (r#"[[ -f Cargo.toml && $HOME == "$PWD" ]]"#, false),
        ("test -3 -eq -3 && [ 10 -gt 2 ]", true),
        ("[[ 10 -lt 2 || -1 -ge 0 ]]", false),
        ("[[ 2 -le 10 && -2 -ne -3 ]]", true),
        (
            r#"test "${SKILLS_MGR_CATALOG_TEST_UNSET_3817:-two words}" = "two words""#,
            true,
        ),
        (r#"[[ ${HOME:-$(./probe 0)} == ~ ]]"#, true),
        (r#"[[ "~" != ~ ]] && test -f ~/config/witness"#, true),
        (r#"cd && [[ "$PWD" == ~ ]] && test -f config/witness"#, true),
        ("cd ~/config && test -f witness", true),
        (r#"cd "$HOME/config" && test -f witness"#, true),
        (
            "lang rust && cd sub && ! lang rust && cd .. && lang rust",
            true,
        ),
        ("(cd sub && test -f nested) && test -f Cargo.toml", true),
        (
            r#"[[ $(cd sub && pwd) == "$PWD/sub" ]] && test -f Cargo.toml"#,
            true,
        ),
        ("{ cd sub; } && test -f nested", true),
        (r#"cd "" || test -f Cargo.toml"#, true),
        ("cd missing || test -f Cargo.toml", true),
        (
            "if lang rust; then test -f Cargo.toml; else false; fi",
            true,
        ),
        ("if lang go; then false; else [ -f Cargo.toml ]; fi", true),
        ("command -v ./probe", true),
        ("lang rust || ./probe 2", true),
        ("[[ -f Cargo.toml || $(./probe 2) == main ]]", true),
    ] {
        let xml = xml(expr).unwrap();
        assert_eq!(xml.contains("name=\"managed\""), visible, "{expr}");
        assert!(!invoked.exists(), "{expr}");
    }

    f.condition("./probe 0");
    assert!(f.manager.is_enabled().unwrap());
    assert!(matches!(
        f.manager.list(),
        Err(Error::UnsupportedCondition(_))
    ));
    assert!(matches!(
        f.manager.get("managed"),
        Err(Error::UnsupportedCondition(_))
    ));
    assert!(!invoked.exists());

    for (expr, visible) in [
        ("[[ $(./probe 2) == main ]]", true),
        (
            r#"[[ ${SKILLS_MGR_CATALOG_TEST_UNSET_3817:-$(./probe 0)} == main ]]"#,
            true,
        ),
        ("lang go || ./probe 0", true),
        ("./probe 1", false),
        ("./probe 2 || true", true),
    ] {
        let xml = xml(expr).unwrap();
        assert_eq!(xml.contains("name=\"managed\""), visible, "{expr}");
        assert!(!xml.contains("main"), "stdout escaped into XML: {xml}");
        assert_eq!(
            fs::read_to_string(&invoked).unwrap().trim_end(),
            f.temp.path().join("project").to_str().unwrap()
        );
        fs::remove_file(&invoked).unwrap();
    }
    assert!(
        matches!(xml("./probe 2"), Err(Error::Condition {skill, source}) if skill == "managed" && source.to_string().contains("status 2"))
    );
    assert!(matches!(
        xml("test not-an-integer -eq 1"),
        Err(Error::Condition { .. })
    ));
    let host_cwd = std::env::current_dir().unwrap();
    assert!(
        xml("cd sub && ./probe 0")
            .unwrap()
            .contains("name=\"managed\"")
    );
    assert_eq!(std::env::current_dir().unwrap(), host_cwd);
    let sub = f.temp.path().join("project/sub");
    assert_eq!(
        fs::read_to_string(sub.join("invoked")).unwrap().trim_end(),
        sub.to_str().unwrap()
    );
}

#[test]
fn native_scan_at_home_only_counts_direct_files() {
    let mut f = Fixture::new();
    let home = f.temp.path().join("home");
    f.manager = SkillsManager::new(&home, &home, f.temp.path().join("cache"));
    f.skill("home/.skills-mgr/skills/managed", "managed", "Managed\n");
    f.write("home/src/index.ts", "");
    f.write("home/pnpm-lock.yaml", "");
    f.assert_condition("tooling pnpm && ! lang ts", true);
    f.assert_condition(r#"[ "$PWD" == "$HOME" ] || has_dependency missing"#, true);
}

#[test]
fn native_dependencies_match_one_declaration_and_all_its_numeric_bounds() {
    let f = Fixture::new();
    f.skill("home/.skills-mgr/skills/managed", "managed", "Managed\n");
    f.write("project/go.mod","module example.com/app\n\nrequire (\n example.com/direct v2.3.1\n example.com/other v2.0.0 // indirect\n)\n");
    f.write("project/web/package.json",r#"{"dependencies":{"@scope/core":"workspace:^2.3.4","split":"1"},"devDependencies":{"dev":"2 || 3"},"optionalDependencies":{"opt":">=2 <4"},"peerDependencies":{"peer":"1"}}"#);
    f.write(
        "project/Cargo.toml",
        r#"
[dependencies]
split="3.1"
[dev-dependencies]
fixture={path="local"}
[build-dependencies]
builder="1"
[workspace.dependencies]
shared={version="~3.4"}
[target.'cfg(unix)'.dependencies]
system="4"
[target.'cfg(unix)'.dev-dependencies]
debug="5"
[target.'cfg(unix)'.build-dependencies]
generate="6"
"#,
    );
    for expr in [
        "has_dependency example.com/direct '==2.3' && ! has_dependency example.com/other",
        "has_dependency '@scope/core' '<2 || >=2 && <=2.3'",
        "has_dependency dev '>=2' && ! has_dependency dev '==2'",
        "has_dependency opt && ! has_dependency opt '<3' && ! has_dependency peer",
        "! has_dependency split '>=2 && <3'",
        "has_dependency fixture && ! has_dependency fixture '>=0'",
        "has_dependency builder && has_dependency shared '==3' && has_dependency system && has_dependency debug && has_dependency generate",
    ] {
        f.assert_condition(expr, true);
    }
    f.condition("has_dependency split '>1'");
    assert!(matches!(f.manager.list(), Err(Error::Condition { .. })));
}

#[test]
fn native_scan_honors_parent_ignores_excludes_and_tracked_exceptions() {
    let f = Fixture::new();
    f.skill("home/.skills-mgr/skills/managed", "managed", "Managed\n");
    f.write("project/.gitignore", "*\n!allowed/\n!allowed/keep.ts\n");
    f.write("project/allowed/keep.ts", "");
    f.write("project/hidden/app.go", "");
    f.write("project/hidden/pnpm-lock.yaml", "");
    f.write(
        "project/hidden/package.json",
        r#"{"dependencies":{"tracked":"2"}}"#,
    );
    f.write("project/excluded.rs", "");
    let git = |args: &[&str]| {
        assert!(
            std::process::Command::new("git")
                .current_dir(f.temp.path())
                .env("GIT_CONFIG_NOSYSTEM", "1")
                .env("GIT_CONFIG_GLOBAL", f.temp.path().join("no-config"))
                .args(args)
                .status()
                .unwrap()
                .success()
        );
    };
    git(&["init", "-q"]);
    git(&["add", "-f", "project/hidden/package.json"]);
    f.assert_condition(
        "lang ts && lang node && has_dependency tracked && ! tooling pnpm && ! lang go",
        true,
    );
    f.write("project/.gitignore", "");
    f.write(".git/info/exclude", "*.rs\n");
    f.assert_condition("! lang rust && lang go", true);
    f.write(".gitignore", "project/\n");
    f.assert_condition("lang node && ! lang ts && ! tooling pnpm", true);
}

#[test]
fn harness_suppression_does_not_change_get() {
    let f = Fixture::new();
    f.skill("project/.agents/skills/shared", "shared", "Shared\n");
    f.skill("home/.codex/skills/.system/native", "native", "Codex\n");
    f.skill(
        "home/.codex/plugins/cache/example/plugin/1.0.0/skills/plug",
        "plug",
        "Plugin\n",
    );
    f.write(
        "project/.skills-mgr.json",
        r#"{"schema_revision":3,"skills":{"native":{"enabled":true},"plug":{"enabled":true}}}"#,
    );

    assert_eq!(f.names(), ["native", "plug", "shared"]);
    assert_eq!(
        f.manager.list_for(Harness::Codex).unwrap()[0].name,
        "shared"
    );
    assert_eq!(f.manager.list_for(Harness::Codex).unwrap().len(), 1);
    assert!(f.manager.list_for(Harness::Grok).unwrap().is_empty());
    assert_eq!(
        f.manager.list_xml(&[Harness::Codex]).unwrap(),
        "<skills>\n  <skill name=\"shared\" description=\"Useful shared skill\"></skill>\n</skills>\n"
    );
    assert_eq!(
        f.manager
            .list_xml(&[Harness::Codex, Harness::Grok])
            .unwrap(),
        "<skills></skills>\n"
    );
    assert_eq!(f.manager.get("native").unwrap(), b"Codex\n");
}

#[test]
fn claude_plugin_files_are_read_without_a_harness_process() {
    let f = Fixture::new();
    let manifest = f.skill("plugin/skills/fixture", "plugin-skill", "From the plugin\n");
    let install = manifest.ancestors().nth(3).unwrap();
    f.write(
        "home/.claude/settings.json",
        r#"{"enabledPlugins":{"example@fixture":true},"permissions":null}"#,
    );
    f.write(
        "project/.claude/settings.json",
        r#"{"permissions":{"deny":null}}"#,
    );
    f.write(
        "home/.claude/plugins/installed_plugins.json",
        serde_json::to_vec(&json!({
            "version": 2,
            "plugins": {"example@fixture":[{"installPath":install}]}
        }))
        .unwrap(),
    );
    assert!(f.manager.list_for(Harness::Claude).unwrap().is_empty());
    f.write(
        "project/.claude/settings.local.json",
        r#"{"permissions":{"deny":["Skill"]}}"#,
    );
    assert_eq!(
        f.manager.list_for(Harness::Claude).unwrap()[0].name,
        "plugin-skill"
    );
    assert_eq!(f.manager.get("plugin-skill").unwrap(), b"From the plugin\n");
}

#[test]
fn selected_remote_wins_over_unmarked_codex_copy() {
    for (scope, lock, selection) in [
        (
            "project",
            "project/.skills-mgr.json",
            r#"{"schema_revision":3,"skills":{"example":{"enabled":true}}}"#,
        ),
        (
            "home",
            "home/.skills-mgr/.skills-mgr.json",
            r#"{"schema_revision":1,"skills":{"example":true}}"#,
        ),
    ] {
        let f = Fixture::new();
        f.write(lock, selection);
        f.skill(
            &format!("{scope}/.codex/skills/example"),
            "example",
            "Stale\n",
        );
        f.write(
            "cache/skills-mgr/remote-skills/entries/5bcce21409e160edd1439c35cada2399a44b43655f9b06a754e274a350345a69.json",
            r#"{"schemaRevision":1,"provider":"repository","id":"example-id","name":"example","locator":"https://example.invalid/example","content":"content/generation","fetchedAt":"2026-08-12T12:00:00Z"}"#,
        );
        assert_eq!(f.manager.get("example").unwrap(), b"Stale\n");
        f.write(
            "cache/skills-mgr/remote-skills/content/generation/SKILL.md",
            "---\nname: example\ndescription: Canonical remote\n---\nRemote\nSecond line\n",
        );

        let listed = f.manager.list().unwrap();
        assert_eq!(listed.len(), 1);
        assert_eq!(listed[0].description, "Canonical remote", "{scope}");
        assert_eq!(
            f.manager.list_for(Harness::Claude).unwrap()[0].description,
            listed[0].description
        );
        assert_eq!(f.manager.get("example").unwrap(), b"Remote\nSecond line\n");
        assert_eq!(
            f.manager.get_lines("example", 2..=2).unwrap(),
            b"Second line\n"
        );

        f.write(
            lock,
            r#"{"schema_revision":3,"skills":{"example":{"enabled":true,"remote":{"provider":"repository","id":"example-id","name":"example","locator":"https://example.invalid/example"}}}}"#,
        );
        f.write(
            "cache/skills-mgr/remote-skills/entries/ed5b01661dd73ccbb3e07f174ffa9d1af7b367f32cac82c2d0a5ade931967201.json",
            r#"{"schemaRevision":1,"provider":"repository","id":"alternate-id","name":"example","locator":"https://example.invalid/alternate","content":"content/alternate","fetchedAt":"2026-08-12T12:00:00Z"}"#,
        );
        f.skill(
            "cache/skills-mgr/remote-skills/content/alternate",
            "example",
            "Unselected provider copy\n",
        );
        assert_eq!(f.manager.list().unwrap()[0].description, "Canonical remote");
        assert_eq!(f.manager.get("example").unwrap(), b"Remote\nSecond line\n");

        f.skill(
            "project/.agents/skills/example",
            "example",
            "Project owner\n",
        );
        assert_eq!(
            f.manager.list().unwrap()[0].description,
            "Useful example skill"
        );
        assert_eq!(f.manager.get("example").unwrap(), b"Project owner\n");
    }
}

#[test]
fn uses_persisted_remote_content_and_applies_local_patches() {
    let f = Fixture::new();
    let root = "cache/skills-mgr/remote-skills";
    let key = "5bcce21409e160edd1439c35cada2399a44b43655f9b06a754e274a350345a69";
    let record = json!({
        "schemaRevision":1, "provider":"repository", "id":"example-id",
        "name":"remote", "locator":"https://example.invalid/example",
        "content":"content/generation", "fetchedAt":"2026-08-12T12:00:00Z",
        "exclude":["unused/"]
    });
    f.write(
        &format!("{root}/entries/{key}.json"),
        serde_json::to_vec(&record).unwrap(),
    );
    f.write(
        "project/.skills-mgr.json",
        r#"{"schema_revision":3,"skills":{"remote":{"enabled":true}}}"#,
    );
    let placeholder = "project/.agents/skills/remote";
    f.skill(placeholder, "remote", "Placeholder\n");
    f.write(
        &format!("{placeholder}/.skills-mgr-placeholder"),
        "skills-mgr remote placeholder\n",
    );
    assert!(f.manager.list().unwrap().is_empty());
    assert!(matches!(f.manager.get("remote"), Err(Error::NotFound(_))));

    let original = b"---\nname: remote\ndescription: From cache\n---\nOriginal";
    f.write(&format!("{root}/content/generation/SKILL.md"), original);
    assert_eq!(f.manager.get("remote").unwrap(), b"Original");

    let path = format!("home/.skills-mgr/skills/.remote-patches/{key}.patch");
    f.write(
        &path,
        "# skills-mgr-base-sha256 ab1de4eddfbe9c820c661cf4fb0ce1bd1d2873639aa2a0fe453d19841ba444fb\n\
        # skills-mgr-result-sha256 4fc537185d30b2b62a0654da03cb946c1cb30ef4a5520b150a4dfaf66312aa2a\n\
        --- a/SKILL.md\n+++ b/SKILL.md\n@@ -1,5 +1,5 @@\n ---\n name: remote\n\
        -description: From cache\n+description: Local edit\n ---\n-Original\n\
        \\ No newline at end of file\n+Patched\n",
    );
    assert_eq!(f.manager.list().unwrap()[0].description, "Local edit");
    let manifest_targets = ["remote", "remote/SKILL.md", "remote/./SKILL.md"];
    for target in manifest_targets {
        assert_eq!(f.manager.get(target).unwrap(), b"Patched\n");
    }
    let manifest = format!("{root}/content/generation/SKILL.md");
    f.write(
        &manifest,
        b"---\nname: remote\ndescription: New provider version\n---\nNew content",
    );
    assert_eq!(
        f.manager.list().unwrap()[0].description,
        "New provider version"
    );
    for target in manifest_targets {
        assert!(matches!(
            f.manager.get(target),
            Err(Error::StaleRemotePatch(_))
        ));
    }
    fs::remove_file(f.temp.path().join(path)).unwrap();
    f.write(
        &format!("{root}/entries/{key}.override"),
        r#"{"schemaRevision":1,"disableModelInvocation":true}"#,
    );
    assert!(f.manager.list().unwrap().is_empty());
    assert!(
        f.manager
            .get("remote")
            .unwrap()
            .ends_with(b"---\nNew content")
    );
}

#[cfg(unix)]
#[test]
fn strict_discovery_does_not_replace_denied_manifest_with_an_alternate() {
    use std::{io, os::unix::fs::PermissionsExt};

    let f = Fixture::new();
    let preferred = f.skill("project/.agents/skills/preferred", "shared", "Project");
    f.skill("home/.agents/skills/alternate", "shared", "Alternate");
    f.write(
        "project/.skills-mgr.json",
        r#"{"schema_revision":1,"skills":{"shared":true}}"#,
    );
    fs::set_permissions(&preferred, fs::Permissions::from_mode(0)).unwrap();
    if fs::File::open(&preferred).is_ok() {
        return; // Privileged users can still read mode-000 files.
    }
    assert_eq!(f.manager.get("shared").unwrap(), b"Alternate");
    let strict = f.manager.with_strict_discovery();
    for error in [
        strict.get("shared").unwrap_err(),
        strict.list_xml(&[Harness::Codex]).unwrap_err(),
    ] {
        assert!(
            matches!(error, Error::Io { path, source } if path == preferred && source.kind() == io::ErrorKind::PermissionDenied)
        );
    }
    fs::remove_file(&preferred).unwrap();
    assert_eq!(strict.get("shared").unwrap(), b"Alternate");
}

#[test]
fn caller_admission_precedes_missing_roots_and_reached_catalog_commands() {
    struct Authority {
        denied: PathBuf,
    }
    impl skills_mgr::QueryAuthority for Authority {
        fn check_read(&self, path: &std::path::Path) -> std::io::Result<()> {
            if path == self.denied {
                return Err(std::io::ErrorKind::PermissionDenied.into());
            }
            Ok(())
        }
        fn execute(
            &self,
            _cwd: &std::path::Path,
            _command: &std::path::Path,
            _: &[String],
            _: bool,
        ) -> std::io::Result<(i32, Vec<u8>)> {
            Err(std::io::ErrorKind::PermissionDenied.into())
        }
    }
    let f = Fixture::new();
    f.skill("home/.agents/skills/alternate", "shared", "Alternate");
    f.write(
        "project/.skills-mgr.json",
        r#"{"schema_revision":1,"skills":{"shared":true}}"#,
    );
    let path = f.temp.path().join("project/.agents/skills");
    let manager = f.manager.with_authority(std::sync::Arc::new(Authority {
        denied: path.clone(),
    }));
    assert!(
        matches!(manager.get("shared"), Err(Error::Io {path: p, source}) if p == path && source.kind() == std::io::ErrorKind::PermissionDenied)
    );
    #[cfg(unix)]
    {
        let f = Fixture::new();
        f.skill("project/.agents/skills/shared", "shared", "Body");
        f.write(
            "project/.skills-mgr.json",
            r#"{"schema_revision":3,"skills":{"shared":{"enabled":"false || /bin/true"}}}"#,
        );
        let manager = f.manager.with_authority(std::sync::Arc::new(Authority {
            denied: PathBuf::new(),
        }));
        assert!(
            matches!(manager.list_xml(&[Harness::Codex]), Err(Error::Condition {source, ..}) if source.kind() == std::io::ErrorKind::PermissionDenied)
        );
    }
}
