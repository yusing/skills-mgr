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
    }
    assert!(!f.temp.path().join("project/unexpected").exists());
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
