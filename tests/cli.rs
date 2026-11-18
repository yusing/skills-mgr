#![cfg(feature = "cli")]

use std::process::Command;

fn cli() -> Command {
    Command::new(env!("CARGO_BIN_EXE_skills-mgr"))
}

#[test]
fn help_and_fish_completion_do_not_need_a_companion() {
    let output = cli().args(["help", "get"]).output().unwrap();
    assert!(output.status.success(), "{output:?}");
    assert!(output.stdout.starts_with(b"Usage:\n\n  skills-mgr get "));
    assert!(output.stderr.is_empty());
    let alias = cli().args(["get", "--help"]).output().unwrap();
    assert_eq!(output.stdout, alias.stdout);

    let error = cli().args(["help", "unknown"]).output().unwrap();
    assert_eq!(error.status.code(), Some(1));
    assert!(error.stdout.is_empty());
    assert!(error.stderr.ends_with(b"unknown help target \"unknown\"\n"));

    let fish = cli().args(["completion", "fish"]).output().unwrap();
    assert!(fish.status.success(), "{fish:?}");
    assert_eq!(fish.stdout, include_bytes!("../completion.fish"));
    assert!(fish.stderr.is_empty());
}

#[cfg(target_os = "linux")]
#[test]
fn info_reports_paths_without_moving_the_legacy_selection() {
    let home = tempfile::tempdir().unwrap();
    let project = home.path().join("project");
    let codex = home.path().join("codex-override");
    let legacy = home.path().join(".skills-mgr.json");
    std::fs::create_dir_all(project.join(".agents/skills")).unwrap();
    std::fs::create_dir_all(codex.join("skills")).unwrap();
    std::fs::write(&legacy, br#"{"schema_revision":3,"skills":{}}"#).unwrap();

    let result = cli()
        .arg("info")
        .current_dir(project)
        .env("HOME", home.path())
        .env("XDG_CACHE_HOME", home.path().join("cache"))
        .env("CODEX_HOME", codex)
        .output()
        .unwrap();
    assert!(result.status.success(), "{result:?}");
    assert!(result.stderr.is_empty());
    let info = String::from_utf8(result.stdout).unwrap();
    assert!(info.contains("  Project shared: ~/project/.agents/skills\n"));
    assert!(info.contains("  User Codex: ~/codex-override/skills\n"));
    assert!(info.contains("  Store: ~/cache/skills-mgr/remote-skills [MISSING]\n"));
    assert!(info.contains("  Global selection: ~/.skills-mgr/.skills-mgr.json [MISSING]\n"));
    assert!(legacy.is_file());
    assert!(!home.path().join(".skills-mgr").exists());
}
