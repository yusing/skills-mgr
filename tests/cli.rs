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

#[cfg(target_os = "linux")]
#[test]
fn list_get_and_completion_share_selection_and_content_without_go() {
    use std::{fs, path::Path};

    let home = tempfile::tempdir().unwrap();
    let project = home.path().join("project");
    let put = |path: &Path, bytes: &[u8]| {
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, bytes).unwrap();
    };
    for (name, markdown) in [
        (
            "alpha",
            "---\nname: alpha\ndescription: 'A & B'\n---\nline one\nline two\n",
        ),
        ("off", "---\nname: off\ndescription: Off\n---\nnot served\n"),
        (
            "shell",
            "---\nname: shell\ndescription: Shell condition\n---\nshell body\n",
        ),
    ] {
        put(
            &project.join(format!(".agents/skills/{name}/SKILL.md")),
            markdown.as_bytes(),
        );
    }
    put(
        &project.join(".agents/skills/alpha/references/query&sort.md"),
        b"notes\n",
    );
    put(
        &project.join(".agents/skills/alpha/guide.md"),
        b"---\ntitle: Hidden header\n---\npublic text\n",
    );
    put(&project.join(".skills-mgr.json"), br#"{"schema_revision":3,"skills":{"off":{"enabled":false},"shell":{"enabled":"lang rust || command -v bash"}}}"#);
    let invoke = |args: &[&str]| {
        cli()
            .args(args)
            .current_dir(&project)
            .env("HOME", home.path())
            .env("XDG_CACHE_HOME", home.path().join("cache"))
            .env("CODEX_HOME", home.path().join("codex"))
            .env_remove("CLAUDECODE")
            .env_remove("GROK_AGENT")
            .env_remove("GROK_SESSION_ID")
            .env_remove("CODEX_THREAD_ID")
            .env("SKILLS_MGR_NO_REFRESH", "1")
            .output()
            .unwrap()
    };
    let list = invoke(&["list"]);
    assert!(list.status.success(), "{list:?}");
    assert!(list.stderr.is_empty());
    assert_eq!(
        String::from_utf8(list.stdout).unwrap(),
        "<skills>\n  <skill name=\"alpha\" description=\"A &amp; B\">\n    <references>guide.md references/'query&amp;sort'.md</references>\n  </skill>\n  <skill name=\"shell\" description=\"Shell condition\"></skill>\n</skills>\n"
    );
    for (args, expected) in [
        (vec!["get", "alpha", "2:3"], "line two\n"),
        (vec!["get", "alpha/guide.md"], "public text\n"),
        (vec!["get", "shell"], "shell body\n"),
        (vec!["__complete-skills", "enabled"], "alpha\nshell\n"),
        (vec!["__complete-skills", "all"], "alpha\noff\nshell\n"),
    ] {
        let output = invoke(&args);
        assert!(output.status.success(), "{args:?}: {output:?}");
        assert_eq!(output.stdout, expected.as_bytes(), "{args:?}");
        assert!(output.stderr.is_empty());
    }
    let error = invoke(&["get", "off"]);
    assert_eq!(error.status.code(), Some(1));
    assert!(error.stdout.is_empty());
    assert!(
        String::from_utf8(error.stderr)
            .unwrap()
            .contains("skill \"off\" is not enabled")
    );

    put(&project.join(".agents/skills/alpha/script.sh"), b"#!/bin/sh\nprintf 'from %s: %s\\n' \"$PWD\" \"$1\"\nprintf 'child stderr\\n' >&2\nexit 23\n");
    use std::os::unix::fs::PermissionsExt;
    let script = project.join(".agents/skills/alpha/script.sh");
    fs::set_permissions(&script, fs::Permissions::from_mode(0o755)).unwrap();
    let run = invoke(&["run", "alpha/script.sh", "two words"]);
    assert_eq!(run.status.code(), Some(23));
    assert_eq!(
        String::from_utf8(run.stdout).unwrap(),
        format!("from {}: two words\n", script.parent().unwrap().display())
    );
    assert_eq!(run.stderr, b"child stderr\n");

    put(
        &project.join(".agents/skills/alpha/references/empty.md"),
        b"---\ntitle: Empty body\n---\n\n",
    );
    let check = invoke(&["check", "alpha", "alpha/script.sh", "alpha/guide.md"]);
    assert_eq!(check.status.code(), Some(1));
    assert_eq!(check.stdout, b"PASS alpha\nPASS alpha/guide.md\nPASS alpha/references/query&sort.md\nPASS alpha/script.sh\n");
    assert!(check.stderr.starts_with(
        b"FAIL alpha/references/empty.md: missing body or frontmatter-only placeholder\n"
    ));
    assert!(
        !String::from_utf8(check.stderr)
            .unwrap()
            .contains("child stderr")
    );
}
