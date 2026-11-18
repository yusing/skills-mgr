#![cfg(feature = "cli")]

use std::process::Command;

fn cli() -> Command {
    let mut cmd = Command::new(env!("CARGO_BIN_EXE_skills-mgr"));
    cmd.env("SKILLS_MGR_NO_REFRESH", "1");
    cmd
}

#[cfg(target_os = "linux")]
fn tui_request(mut cmd: Command, input: serde_json::Value) -> std::process::Output {
    use std::{io::Write, process::Stdio};
    let mut child = cmd
        .arg("__tui")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    child
        .stdin
        .take()
        .unwrap()
        .write_all(input.to_string().as_bytes())
        .unwrap();
    child.wait_with_output().unwrap()
}

#[cfg(target_os = "linux")]
#[test]
fn set_updates_the_selected_layer_and_owned_placeholders_without_go() {
    use std::fs;

    let home = tempfile::tempdir().unwrap();
    let project = home.path().join("project");
    let manager = home.path().join(".skills-mgr");
    let skill = manager.join("skills/alpha");
    let manifest = "---\nname: alpha\ndescription: Test skill\n---\nA body.\n";
    fs::create_dir_all(&project).unwrap();
    fs::create_dir_all(&skill).unwrap();
    fs::write(skill.join("SKILL.md"), manifest).unwrap();
    let project_selection = project.join(".skills-mgr.json");
    let global_selection = manager.join(".skills-mgr.json");
    let legacy = home.path().join(".skills-mgr.json");
    fs::write(&legacy, br#"{"schema_revision":1,"skills":{"alpha":true}}"#).unwrap();
    let run = |cwd: &std::path::Path, args: &[&str]| {
        cli()
            .current_dir(cwd)
            .env("HOME", home.path())
            .env("CODEX_HOME", home.path().join(".codex"))
            .env("XDG_CACHE_HOME", home.path().join("cache"))
            .args(args)
            .output()
            .unwrap()
    };
    let read_json = |path: &std::path::Path| -> serde_json::Value {
        serde_json::from_slice(&fs::read(path).unwrap()).unwrap()
    };
    let outcome = run(home.path(), &["set", "alpha", "true"]);
    assert!(outcome.status.success(), "{outcome:?}");
    assert_eq!(outcome.stdout, b"Updated alpha (global selection)\n");
    assert!(!legacy.exists());
    assert_eq!(read_json(&global_selection)["schema_revision"], 3);
    assert_eq!(
        read_json(&global_selection)["skills"]["alpha"]["enabled"],
        true
    );

    fs::write(
        &project_selection,
        br#"{"schema_revision":2,"skills":{"unrelated":{"enabled":false}}}"#,
    )
    .unwrap();
    let condition = "  test -f marker  ";
    let outcome = run(&project, &["set", "alpha", condition]);
    assert!(outcome.status.success(), "{outcome:?}");
    let selection = read_json(&project_selection);
    assert_eq!(selection["skills"]["alpha"]["enabled"], condition);
    assert_eq!(selection["skills"]["unrelated"]["enabled"], false);
    let disabled = run(&project, &["get", "alpha"]);
    assert!(!disabled.status.success());
    fs::write(project.join("marker"), "").unwrap();
    assert_eq!(run(&project, &["get", "alpha"]).stdout, b"A body.\n");
    for harness in [".agents", ".claude"] {
        let dir = project.join(harness).join("skills/alpha");
        let stub = fs::read_to_string(dir.join("SKILL.md")).unwrap();
        assert!(stub.contains("disable-model-invocation: true"));
        assert!(!stub.contains("A body."));
        assert_eq!(
            fs::read(dir.join(".skills-mgr-placeholder")).unwrap(),
            b"skills-mgr remote placeholder\n"
        );
    }

    let before = fs::read(&project_selection).unwrap();
    for args in [["set", "alpha", "if then"], ["set", "missing", "true"]] {
        let outcome = run(&project, &args);
        assert!(!outcome.status.success(), "{outcome:?}");
        assert!(outcome.stdout.is_empty());
        assert_eq!(fs::read(&project_selection).unwrap(), before);
    }
    let outcome = run(&project, &["set", "alpha", "false"]);
    assert!(outcome.status.success(), "{outcome:?}");
    let agent_skills = project.join(".agents/skills/alpha");
    let claude_skills = project.join(".claude/skills/alpha");
    assert!(!agent_skills.exists());
    assert!(!claude_skills.exists());
    fs::create_dir_all(&claude_skills).unwrap();
    fs::write(claude_skills.join("SKILL.md"), "Unowned notes").unwrap();
    let before = fs::read(&project_selection).unwrap();
    let collision = run(&project, &["set", "alpha", "inherit"]);
    assert!(!collision.status.success(), "{collision:?}");
    assert!(!agent_skills.exists());
    assert_eq!(fs::read(&project_selection).unwrap(), before);
    assert_eq!(
        fs::read(claude_skills.join("SKILL.md")).unwrap(),
        b"Unowned notes"
    );
    fs::remove_file(claude_skills.join("SKILL.md")).unwrap();
    let outcome = run(&project, &["set", "alpha", "inherit"]);
    assert!(outcome.status.success(), "{outcome:?}");
    assert!(
        read_json(&project_selection)["skills"]
            .get("alpha")
            .is_none()
    );
    assert!(agent_skills.join("SKILL.md").is_file());
    assert!(claude_skills.join("SKILL.md").is_file());
    assert_eq!(run(&project, &["get", "alpha"]).stdout, b"A body.\n");
    assert_eq!(
        fs::read_to_string(skill.join("SKILL.md")).unwrap(),
        manifest
    );
}

#[cfg(target_os = "linux")]
#[test]
fn tui_selection_requests_reject_stale_owners_and_return_configured_values() {
    use std::fs;

    let home = tempfile::tempdir().unwrap();
    let project = home.path().join("project");
    let skill = project.join(".agents/skills/alpha");
    let path = skill.join("SKILL.md");
    let selection = project.join(".skills-mgr.json");
    fs::create_dir_all(&skill).unwrap();
    fs::write(&path, "---\nname: alpha\ndescription: Alpha\n---\nbody\n").unwrap();
    let request = |input: serde_json::Value| {
        let mut cmd = cli();
        cmd.current_dir(&project)
            .env("HOME", home.path())
            .env("CODEX_HOME", home.path().join(".codex"))
            .env("XDG_CACHE_HOME", home.path().join("cache"));
        tui_request(cmd, input)
    };
    let toggle = |path: &std::path::Path| {
        request(serde_json::json!({"op":"toggle", "global":false, "name":"alpha", "path":path}))
    };
    let stale = toggle(&skill.join("moved.md"));
    assert!(!stale.status.success());
    assert!(stale.stdout.is_empty());
    assert!(!selection.exists());

    let toggled = toggle(&path);
    assert!(toggled.status.success(), "{toggled:?}");
    assert_eq!(toggled.stdout, b"false\n", "project skill starts enabled");
    let edited = request(serde_json::json!({
        "op":"set_enabled", "global":false, "name":"alpha", "path":path, "value":"test -f marker"
    }));
    assert!(edited.status.success(), "{edited:?}");
    let response: serde_json::Value = serde_json::from_slice(&edited.stdout).unwrap();
    assert_eq!(response["ProjectExpressions"]["alpha"], "test -f marker");
    let configured: serde_json::Value =
        serde_json::from_slice(&fs::read(selection).unwrap()).unwrap();
    assert_eq!(configured["skills"]["alpha"]["enabled"], "test -f marker");
}

#[cfg(target_os = "linux")]
#[test]
fn local_edit_preserves_the_manifest_owner_and_moves_its_selection() {
    use std::{fs, os::unix::fs::PermissionsExt};

    let home = tempfile::tempdir().unwrap();
    let project = home.path().join("project");
    let skill = home.path().join(".skills-mgr/skills/alpha");
    let manifest = skill.join("SKILL.md");
    let input = home.path().join("draft.md");
    let selection = project.join(".skills-mgr.json");
    fs::create_dir_all(&project).unwrap();
    fs::create_dir_all(&skill).unwrap();
    let original = "---\nname: alpha\ndescription: Before\n---\nOriginal.\n";
    fs::write(&manifest, original).unwrap();
    fs::set_permissions(&manifest, fs::Permissions::from_mode(0o640)).unwrap();
    let run = |args: &[&str]| {
        cli()
            .current_dir(&project)
            .env("HOME", home.path())
            .env("CODEX_HOME", home.path().join(".codex"))
            .env("XDG_CACHE_HOME", home.path().join("cache"))
            .args(args)
            .output()
            .unwrap()
    };
    let set = run(&["set", "alpha", "true"]);
    assert!(set.status.success(), "{set:?}");
    let inspected = run(&["inspect", "alpha"]);
    assert!(inspected.status.success(), "{inspected:?}");
    let report: serde_json::Value = serde_json::from_slice(&inspected.stdout).unwrap();
    let digest = report["resolved"]["sha256"]
        .as_str()
        .unwrap()
        .to_uppercase();
    let edit = |digest: &str| {
        run(&[
            "edit",
            "alpha",
            "--file",
            input.to_str().unwrap(),
            "--expect-sha256",
            digest,
        ])
    };
    let revised = "---\nname: beta\ndescription: After\n---\nRevised.\n";
    fs::write(&input, revised).unwrap();
    let before = fs::read(&selection).unwrap();
    let stale = edit(&"0".repeat(64));
    assert!(!stale.status.success());
    assert!(stale.stdout.is_empty());
    assert_eq!(fs::read(&selection).unwrap(), before);
    assert_eq!(fs::read_to_string(&manifest).unwrap(), original);
    let conflict = project.join(".claude/skills/beta");
    fs::create_dir_all(&conflict).unwrap();
    fs::write(conflict.join("SKILL.md"), "Unmanaged notes").unwrap();
    let blocked = edit(&digest);
    assert!(!blocked.status.success());
    assert_eq!(fs::read(&selection).unwrap(), before);
    assert_eq!(fs::read_to_string(&manifest).unwrap(), original);
    assert!(!project.join(".agents/skills/beta").exists());
    assert!(project.join(".agents/skills/alpha/SKILL.md").exists());
    assert_eq!(
        fs::read(conflict.join("SKILL.md")).unwrap(),
        b"Unmanaged notes"
    );
    fs::remove_file(conflict.join("SKILL.md")).unwrap();

    let edited = edit(&digest);
    assert!(edited.status.success(), "{edited:?}");
    assert_eq!(edited.stdout, b"Updated beta\n");
    assert_eq!(fs::read_to_string(&manifest).unwrap(), revised);
    assert_eq!(
        fs::metadata(&manifest).unwrap().permissions().mode() & 0o777,
        0o640
    );
    assert_eq!(run(&["get", "beta"]).stdout, b"Revised.\n");
    assert!(project.join(".agents/skills/beta/SKILL.md").is_file());
    assert!(!project.join(".agents/skills/alpha").exists());
    let selection: serde_json::Value =
        serde_json::from_slice(&fs::read(selection).unwrap()).unwrap();
    assert_eq!(selection["skills"]["beta"]["enabled"], true);
    assert!(selection["skills"].get("alpha").is_none());
}

#[cfg(target_os = "linux")]
#[test]
fn adopt_moves_shared_skills_as_one_transaction() {
    use std::fs;

    let home = tempfile::tempdir().unwrap();
    let project = home.path().join("project");
    let user = home.path().join(".agents/skills");
    let managed = home.path().join(".skills-mgr/skills");
    let project_alpha = project.join(".agents/skills/alpha");
    fs::create_dir_all(&project_alpha).unwrap();
    fs::write(
        project_alpha.join("SKILL.md"),
        "---\nname: alpha\ndescription: Project owner\n---\nLocal.\n",
    )
    .unwrap();
    for name in ["alpha", "beta"] {
        let dir = user.join(name);
        fs::create_dir_all(&dir).unwrap();
        fs::write(
            dir.join("SKILL.md"),
            format!("---\nname: {name}\ndescription: Shared owner\n---\nShared.\n"),
        )
        .unwrap();
        fs::write(dir.join("guide.md"), "A sidecar.").unwrap();
    }
    fs::create_dir_all(managed.join("beta")).unwrap();
    fs::write(
        home.path().join(".skills-mgr/.skills-mgr.json"),
        br#"{"schema_revision":3,"skills":{"alpha":{"enabled":true},"beta":{"enabled":true}}}"#,
    )
    .unwrap();
    let adopt = || {
        cli()
            .current_dir(&project)
            .env("HOME", home.path())
            .env("CODEX_HOME", home.path().join(".codex"))
            .env("XDG_CACHE_HOME", home.path().join("cache"))
            .arg("adopt")
            .output()
            .unwrap()
    };
    let collision = adopt();
    assert!(!collision.status.success(), "{collision:?}");
    assert!(collision.stdout.is_empty());
    assert!(user.join("alpha/guide.md").is_file());
    assert!(user.join("beta/guide.md").is_file());
    assert!(!managed.join("alpha").exists());
    fs::remove_dir(managed.join("beta")).unwrap();
    let adopted = adopt();
    assert!(adopted.status.success(), "{adopted:?}");
    assert_eq!(adopted.stdout, b"alpha\nbeta\n");
    for name in ["alpha", "beta"] {
        assert_eq!(
            fs::read(managed.join(name).join("guide.md")).unwrap(),
            b"A sidecar."
        );
        for harness in [".agents", ".claude"] {
            assert!(
                home.path()
                    .join(harness)
                    .join("skills")
                    .join(name)
                    .join(".skills-mgr-placeholder")
                    .is_file()
            );
        }
    }
    assert!(
        fs::read_to_string(project_alpha.join("SKILL.md"))
            .unwrap()
            .contains("Local.")
    );
    assert!(!project.join(".skills-mgr.json").exists());
}

#[cfg(target_os = "linux")]
#[test]
fn remote_edit_saves_and_replaces_only_the_local_patch() {
    use sha2::{Digest, Sha256};
    use std::fs;

    let home = tempfile::tempdir().unwrap();
    let project = home.path().join("project");
    let cache = home.path().join("cache");
    let store = cache.join("skills-mgr/remote-skills");
    let root = store.join("content/alpha");
    let id = "owner/repo/alpha";
    let digest = |bytes: &[u8]| {
        Sha256::digest(bytes)
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>()
    };
    let key = digest(format!("skills.sh\0{id}").as_bytes());
    let patch = home
        .path()
        .join(".skills-mgr/skills/.remote-patches")
        .join(format!("{key}.patch"));
    let manifest = root.join("SKILL.md");
    let draft = home.path().join("draft.md");
    let original = b"---\nname: alpha\ndescription: Original wording\n---\nProvider instructions";
    let edited = b"---\nname: alpha\ndescription: Edited wording\n---\nLocal instructions";
    fs::create_dir_all(&project).unwrap();
    fs::create_dir_all(&root).unwrap();
    fs::create_dir_all(store.join("entries")).unwrap();
    fs::write(&manifest, original).unwrap();
    fs::write(
        store.join("entries").join(format!("{key}.json")),
        serde_json::json!({
            "schemaRevision":1, "provider":"skills.sh", "id":id, "name":"alpha", "locator":id,
            "fetchedAt":"2026-11-18T00:00:00Z", "content":"content/alpha"
        })
        .to_string(),
    )
    .unwrap();
    let run = |args: &[&str]| {
        cli()
            .current_dir(&project)
            .env("HOME", home.path())
            .env("CODEX_HOME", home.path().join(".codex"))
            .env("XDG_CACHE_HOME", &cache)
            .args(args)
            .output()
            .unwrap()
    };
    let select = run(&["set", "alpha", "true"]);
    assert!(select.status.success(), "{select:?}");
    let edit = |digest: &str| {
        run(&[
            "edit",
            "alpha",
            "--file",
            draft.to_str().unwrap(),
            "--expect-sha256",
            digest,
        ])
    };
    let base = digest(original);
    let edited_digest = digest(edited);
    fs::write(&draft, edited).unwrap();
    let save = edit(&base);
    assert!(save.status.success(), "{save:?}");
    let diff = fs::read_to_string(&patch).unwrap();
    assert!(diff.contains("--- a/SKILL.md\n+++ b/SKILL.md\n"));
    assert!(diff.contains("-Provider instructions\n"));
    assert!(diff.contains("+Local instructions\n"));
    assert_eq!(fs::read(&manifest).unwrap(), original);
    let read = run(&["get", "alpha"]);
    assert!(read.status.success(), "{read:?}");
    assert_eq!(read.stdout, b"Local instructions");
    assert!(String::from_utf8_lossy(&run(&["list"]).stdout).contains("Edited wording"));
    assert!(!edit(&base).status.success(), "stale concurrent draft");
    assert_eq!(fs::read_to_string(&patch).unwrap(), diff);
    fs::write(
        &draft,
        b"---\nname: renamed\ndescription: Cannot rename\n---\nBody\n",
    )
    .unwrap();
    assert!(
        !edit(&edited_digest).status.success(),
        "identity-changing draft"
    );
    assert_eq!(fs::read_to_string(&patch).unwrap(), diff);

    fs::write(&draft, original).unwrap();
    let revert = edit(&edited_digest);
    assert!(revert.status.success(), "{revert:?}");
    assert!(!patch.exists());
    assert_eq!(fs::read(&manifest).unwrap(), original);
}

#[cfg(target_os = "linux")]
#[test]
fn repository_lifecycle_preserves_selection_and_exclusions() {
    use std::{fs, os::unix::fs::PermissionsExt};

    let scratch = tempfile::tempdir().unwrap();
    let home = scratch.path().join("home");
    let project = scratch.path().join("project");
    let repo = scratch.path().join("repository");
    let skill = repo.join("skills/alpha");
    fs::create_dir_all(&home).unwrap();
    fs::create_dir_all(&project).unwrap();
    for directory in ["examples", "scripts"] {
        fs::create_dir_all(skill.join(directory)).unwrap();
    }
    fs::write(
        skill.join("SKILL.md"),
        b"---\nname: alpha\ndescription: Remote Alpha\n---\nInstalled instructions.\n",
    )
    .unwrap();
    fs::write(skill.join("examples/demo.md"), b"Demo.\n").unwrap();
    fs::write(
        skill.join("scripts/tool.sh"),
        b"#!/bin/sh\necho not-run-on-install\n",
    )
    .unwrap();
    fs::set_permissions(
        skill.join("scripts/tool.sh"),
        fs::Permissions::from_mode(0o755),
    )
    .unwrap();
    let isolated_git = |cmd: &mut Command| {
        cmd.env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_SYSTEM", "/dev/null")
            .env_remove("GIT_CONFIG_COUNT")
            .env_remove("GIT_DIR")
            .env_remove("GIT_WORK_TREE")
            .env_remove("GIT_INDEX_FILE");
    };
    let git = |args: &[&str]| {
        let mut cmd = Command::new("git");
        isolated_git(&mut cmd);
        let output = cmd.current_dir(&repo).args(args).output().unwrap();
        assert!(output.status.success(), "{output:?}");
    };
    git(&["init", "--quiet", "--initial-branch=main"]);
    git(&["add", "."]);
    git(&[
        "-c",
        "user.name=Fixture",
        "-c",
        "user.email=fixture@example.invalid",
        "commit",
        "--quiet",
        "-m",
        "fixture",
    ]);
    let address = "https://github.com/fixture/demo";
    let remote_cli = || {
        let mut cmd = cli();
        isolated_git(&mut cmd);
        cmd.current_dir(&project)
            .env("HOME", &home)
            .env("CODEX_HOME", home.join(".codex"))
            .env("XDG_CACHE_HOME", home.join("cache"))
            .env("GIT_ALLOW_PROTOCOL", "file")
            .env("GIT_CONFIG_COUNT", "2")
            .env(
                "GIT_CONFIG_KEY_0",
                format!("url.file://{}.insteadOf", repo.display()),
            )
            .env("GIT_CONFIG_VALUE_0", format!("{address}.git"))
            .env("GIT_CONFIG_KEY_1", "protocol.file.allow")
            .env("GIT_CONFIG_VALUE_1", "always");
        cmd
    };
    let run = |args: &[&str]| remote_cli().args(args).output().unwrap();
    let invalid = run(&["install", "--exclude", "[", address]);
    assert!(!invalid.status.success());
    assert!(invalid.stdout.is_empty());
    assert!(!home.join("cache").exists());
    let installed = run(&["install", "--exclude", "examples", address, "alpha"]);
    assert!(installed.status.success(), "{installed:?}");
    assert_eq!(installed.stdout, b"Installed alpha (project selection)\n");
    assert_eq!(run(&["get", "alpha"]).stdout, b"Installed instructions.\n");
    assert!(!run(&["get", "alpha/examples/demo.md"]).status.success());
    assert!(run(&["get", "alpha/scripts/tool.sh"]).status.success());
    let config = project.join(".skills-mgr.json");
    let before = fs::read(&config).unwrap();
    let json: serde_json::Value = serde_json::from_slice(&before).unwrap();
    assert_eq!(json["skills"]["alpha"]["remote"]["locator"], address);
    assert!(
        project
            .join(".claude/skills/alpha/.skills-mgr-placeholder")
            .is_file()
    );
    let failed = run(&["install", "--exclude", "SKILL.md", address, "alpha"]);
    assert!(!failed.status.success(), "{failed:?}");
    assert_eq!(fs::read(&config).unwrap(), before);
    assert_eq!(run(&["get", "alpha"]).stdout, b"Installed instructions.\n");
    assert!(!run(&["get", "alpha/examples/demo.md"]).status.success());
    let reinstalled = run(&["install", address]);
    assert!(reinstalled.status.success(), "{reinstalled:?}");
    assert_eq!(run(&["get", "alpha/examples/demo.md"]).stdout, b"Demo.\n");

    let installed = run(&["install", "-g", "--exclude", "examples", address, "alpha"]);
    assert!(installed.status.success(), "{installed:?}");
    let manager = home.join(".skills-mgr");
    let global = manager.join(".skills-mgr.json");
    let mut global_selection: serde_json::Value =
        serde_json::from_slice(&fs::read(&global).unwrap()).unwrap();
    global_selection["skills"]["alpha"]["enabled"] = serde_json::json!("exit 23");
    fs::write(&global, serde_json::to_vec(&global_selection).unwrap()).unwrap();
    fs::write(
        &config,
        br#"{"schema_revision":1,"skills":{"alpha":false}}"#,
    )
    .unwrap();
    let failed_uninstall = run(&["uninstall", "alpha"]);
    assert!(!failed_uninstall.status.success(), "{failed_uninstall:?}");
    assert!(String::from_utf8_lossy(&failed_uninstall.stderr).contains("configured globally"));

    let cache = home.join("cache/skills-mgr/remote-skills");
    let entry_path = fs::read_dir(cache.join("entries"))
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .find(|path| path.extension().is_some_and(|ext| ext == "json"))
        .unwrap();
    let record: serde_json::Value =
        serde_json::from_slice(&fs::read(&entry_path).unwrap()).unwrap();
    let content = cache.join(record["content"].as_str().unwrap());
    fs::remove_dir_all(&content).unwrap();
    let legacy = fs::read(&config).unwrap();
    let before_global = fs::read(&global).unwrap();
    fs::rename(&repo, scratch.path().join("unavailable")).unwrap();
    let failed = run(&["sync"]);
    assert!(!failed.status.success(), "{failed:?}");
    assert!(String::from_utf8_lossy(&failed.stderr).contains("alpha"));
    assert_eq!(fs::read(&config).unwrap(), legacy);
    assert_eq!(fs::read(&global).unwrap(), before_global);
    fs::rename(scratch.path().join("unavailable"), &repo).unwrap();

    let restored = run(&["sync"]);
    assert!(restored.status.success(), "{restored:?}");
    assert_eq!(restored.stdout, b"alpha\n");
    let record: serde_json::Value =
        serde_json::from_slice(&fs::read(&entry_path).unwrap()).unwrap();
    let content = cache.join(record["content"].as_str().unwrap());
    assert!(content.join("scripts/tool.sh").is_file());
    assert!(!content.join("examples/demo.md").exists());
    let after: serde_json::Value = serde_json::from_slice(&fs::read(&config).unwrap()).unwrap();
    assert_eq!(after["skills"]["alpha"]["enabled"], false);
    assert_eq!(
        after["skills"]["alpha"]["remote"],
        global_selection["skills"]["alpha"]["remote"]
    );
    assert_eq!(fs::read(&global).unwrap(), before_global);
    let mut stale = record;
    stale["fetchedAt"] = serde_json::json!("2020-01-01T00:00:00Z");
    fs::write(&entry_path, serde_json::to_vec(&stale).unwrap()).unwrap();
    seed_registry_topics(cache.parent().unwrap());
    let refreshed = run(&["refresh-runner"]);
    assert!(refreshed.status.success(), "{refreshed:?}");
    let record: serde_json::Value =
        serde_json::from_slice(&fs::read(&entry_path).unwrap()).unwrap();
    assert_ne!(record["content"], stale["content"]);
    let content = cache.join(record["content"].as_str().unwrap());
    assert!(!content.join("examples/demo.md").exists());
    assert_eq!(fs::read(&global).unwrap(), before_global);
    fs::rename(&repo, scratch.path().join("unavailable")).unwrap();
    assert_eq!(run(&["sync"]).stdout, b"alpha\n");

    let malformed = entry_path.with_extension("override");
    fs::create_dir(&malformed).unwrap();
    fs::write(malformed.join("not-json"), b"malformed override").unwrap();
    fs::remove_dir_all(&content).unwrap();
    let uninstalled = run(&["uninstall", "-g", "alpha"]);
    assert!(uninstalled.status.success(), "{uninstalled:?}");
    assert_eq!(uninstalled.stdout, b"Uninstalled alpha\n");
    assert!(!entry_path.exists());
    assert!(!malformed.exists());
    let key = entry_path.file_stem().unwrap().to_str().unwrap();
    assert!(
        !manager
            .join(format!(".remote-patches/{key}.exclude"))
            .exists()
    );
    assert!(
        !home
            .join(".claude/skills/alpha/.skills-mgr-placeholder")
            .exists()
    );
    let after_global: serde_json::Value =
        serde_json::from_slice(&fs::read(&global).unwrap()).unwrap();
    assert!(after_global["skills"].as_object().unwrap().is_empty());

    fs::rename(scratch.path().join("unavailable"), &repo).unwrap();
    for (provider, id, locator) in [
        ("skills.sh", "fixture/demo/alpha", "fixture/demo/alpha"),
        (
            "SkillsMP",
            "fixture-alpha",
            "https://github.com/fixture/demo/tree/main/skills/alpha?tab=readme",
        ),
    ] {
        let locks = serde_json::json!({
            "schema_revision": 3,
            "skills": {
                "alpha": {"enabled": "test -d .", "remote": {
                    "provider": provider, "id": id, "name": "alpha", "locator": locator
                }},
                "skip": {"enabled": false, "remote": {
                    "provider": "skills.sh", "id": "not-a-repository", "name": "skip",
                    "locator": "not-a-repository"
                }}
            }
        });
        let before = serde_json::to_vec(&locks).unwrap();
        fs::write(&config, &before).unwrap();
        let synced = run(&["sync"]);
        assert!(synced.status.success(), "{provider}: {synced:?}");
        assert_eq!(synced.stdout, b"alpha\n");
        assert_eq!(fs::read(&config).unwrap(), before);
        assert_eq!(run(&["get", "alpha"]).stdout, b"Installed instructions.\n");
        let removed = run(&["uninstall", "alpha"]);
        assert!(removed.status.success(), "{provider}: {removed:?}");
    }

    let reference = serde_json::json!({
        "provider":"skills.sh", "id":"fixture/demo/alpha",
        "name":"alpha", "locator":"fixture/demo/alpha"
    });
    let toggle = || {
        tui_request(
            remote_cli(),
            serde_json::json!({
                "op":"toggle_remote", "global":false, "reference": reference
            }),
        )
    };
    let enabled = toggle();
    assert!(enabled.status.success(), "{enabled:?}");
    let snapshot: serde_json::Value = serde_json::from_slice(&enabled.stdout).unwrap();
    let row = &snapshot["Skills"][0];
    assert_eq!(row["Name"], "alpha");
    assert_eq!(snapshot["Enabled"], true);
    let key = row["RemoteKey"].as_str().unwrap();
    let path = row["Path"].as_str().unwrap();
    let disabled = toggle();
    assert!(disabled.status.success(), "{disabled:?}");
    let snapshot: serde_json::Value = serde_json::from_slice(&disabled.stdout).unwrap();
    assert_eq!(snapshot["Enabled"], false);
    let edited = tui_request(
        remote_cli(),
        serde_json::json!({
            "op":"set_enabled", "global":false, "name":"alpha", "key":key, "path":path,
            "value": "test -d ."
        }),
    );
    assert!(edited.status.success(), "{edited:?}");
    assert_eq!(run(&["get", "alpha"]).stdout, b"Installed instructions.\n");
    let record_path = cache.join(format!("entries/{key}.json"));
    let record: serde_json::Value =
        serde_json::from_slice(&fs::read(record_path).unwrap()).unwrap();
    fs::remove_dir_all(cache.join(record["content"].as_str().unwrap())).unwrap();
    let missing = tui_request(
        remote_cli(),
        serde_json::json!({
            "op":"toggle", "global":false, "name":"alpha", "key":key
        }),
    );
    assert!(missing.status.success(), "{missing:?}");
    assert_eq!(missing.stdout, b"false\n");
    let removed = tui_request(
        remote_cli(),
        serde_json::json!({
            "op":"uninstall", "global":false, "name":"alpha", "key":key
        }),
    );
    assert!(removed.status.success(), "{removed:?}");
    let snapshot: serde_json::Value = serde_json::from_slice(&removed.stdout).unwrap();
    assert_eq!(snapshot["Skills"], serde_json::json!([]));
}

#[cfg(target_os = "linux")]
fn seed_registry_topics(cache: &std::path::Path) {
    std::fs::write(
        cache.join("skills-sh.json"),
        serde_json::to_vec(&serde_json::json!({
            "schemaRevision": 1, "updatedAt": jiff::Timestamp::now().to_string(), "topics": []
        }))
        .unwrap(),
    )
    .unwrap();
}

#[cfg(target_os = "linux")]
#[test]
fn grok_toggle_edits_only_native_disabled_configuration() {
    use std::{fs, os::unix::fs::PermissionsExt};
    let home = tempfile::tempdir().unwrap();
    let project = home.path().join("project");
    let config = home.path().join(".grok/config.toml");
    fs::create_dir(&project).unwrap();
    fs::create_dir(config.parent().unwrap()).unwrap();
    let request = || {
        let mut cmd = cli();
        cmd.current_dir(&project)
            .env("HOME", home.path())
            .env("XDG_CACHE_HOME", home.path().join("cache"));
        tui_request(
            cmd,
            serde_json::json!({"op":"toggle_grok", "global":false, "name":"alpha"}),
        )
    };
    let original = "# retained\nskills = { disabled = [\"alpha\", \"other\"] }\n\n[other]\nvalue = 1 # retained\n";
    fs::write(&config, original).unwrap();
    fs::set_permissions(&config, fs::Permissions::from_mode(0o640)).unwrap();
    let enabled = request();
    assert!(enabled.status.success(), "{enabled:?}");
    assert_eq!(enabled.stdout, b"true\n");
    let updated = fs::read_to_string(&config).unwrap();
    let structure = |text: &str| {
        let (prefix, contents) = text.split_once("disabled = [").unwrap();
        let (_, suffix) = contents.split_once(']').unwrap();
        format!("{prefix}{suffix}")
    };
    assert_eq!(structure(&updated), structure(original));
    assert_eq!(
        updated.parse::<toml::Table>().unwrap()["skills"]["disabled"]
            .as_array()
            .unwrap()[0]
            .as_str(),
        Some("other")
    );
    assert_eq!(
        fs::metadata(&config).unwrap().permissions().mode() & 0o777,
        0o640
    );
    assert!(!project.join(".skills-mgr.json").exists());
    let disabled = request();
    assert!(disabled.status.success(), "{disabled:?}");
    assert_eq!(disabled.stdout, b"false\n");
    let parsed = fs::read_to_string(&config)
        .unwrap()
        .parse::<toml::Table>()
        .unwrap();
    assert_eq!(
        parsed["skills"]["disabled"].as_array().unwrap()[1].as_str(),
        Some("alpha")
    );

    let malformed = b"[skills]\ndisabled = 5\n";
    fs::write(&config, malformed).unwrap();
    assert!(!request().status.success());
    assert_eq!(fs::read(&config).unwrap(), malformed);
}

#[cfg(target_os = "linux")]
#[test]
fn detached_refresh_repairs_placeholders_without_synchronizing_selections() {
    use std::{
        fs, thread,
        time::{Duration, Instant},
    };

    let home = tempfile::tempdir().unwrap();
    let project = home.path().join("project");
    let manager = home.path().join(".skills-mgr");
    let skill = manager.join("skills/managed");
    let cache = home.path().join("cache/skills-mgr");
    for directory in [&project, &skill, &cache] {
        fs::create_dir_all(directory).unwrap();
    }
    fs::write(
        skill.join("SKILL.md"),
        "---\nname: managed\ndescription: Repair me\n---\nbody\n",
    )
    .unwrap();
    let selection = manager.join(".skills-mgr.json");
    let before = br#"{"schema_revision":3,"skills":{
      "managed":{"enabled":true},
      "missing":{"enabled":true,"remote":{"provider":"skills.sh","id":"example/repo/missing","name":"missing","locator":"example/repo/missing"}}
    }}"#;
    fs::write(&selection, before).unwrap();
    seed_registry_topics(&cache);
    let run = |args: &[&str]| {
        cli()
            .env_remove("SKILLS_MGR_NO_REFRESH")
            .env_remove("_SKILLS_MGR_REFRESH_FD")
            .current_dir(&project)
            .env("HOME", home.path())
            .env("CODEX_HOME", home.path().join(".codex"))
            .env("XDG_CACHE_HOME", cache.parent().unwrap())
            .env("GIT_ALLOW_PROTOCOL", "")
            .args(args)
            .output()
            .unwrap()
    };
    assert!(run(&["info"]).status.success());
    let stamp = cache.join("refresh.completed");
    assert!(!stamp.exists());
    let list = run(&["list"]);
    assert!(list.status.success(), "{list:?}");
    let deadline = Instant::now() + Duration::from_secs(4);
    while !stamp.exists() && Instant::now() < deadline {
        thread::sleep(Duration::from_millis(10));
    }
    let completed = fs::read(&stamp).unwrap();
    assert!(
        home.path()
            .join(".claude/skills/managed/.skills-mgr-placeholder")
            .is_file()
    );
    assert!(!cache.join("remote-skills").exists());
    assert_eq!(fs::read(&selection).unwrap(), before);
    assert_eq!(run(&["get", "managed"]).stdout, b"body\n");
    assert_eq!(fs::read(&stamp).unwrap(), completed);
}

#[cfg(target_os = "linux")]
#[test]
fn registry_bridge_reuses_skillsmp_search_cache() {
    use std::fs;
    let home = tempfile::tempdir().unwrap();
    let cache = home.path().join("cache/skills-mgr");
    fs::create_dir_all(&cache).unwrap();
    fs::write(cache.join("skillsmp-search.json"), serde_json::to_vec(&serde_json::json!({
        "schemaRevision": 1,
        "searches": {
            "some query": {
                "updatedAt": jiff::Timestamp::now().to_string(),
                "skills": [
                    {"id":"fixture-id","name":"fixture","description":"fixture result","author":"fixture-owner","githubUrl":"https://github.com/fixture/repo","stars":42},
                    {"id":"duplicate-name","name":"fixture","author":"fixture-owner"}
                ]
            }
        }
    })).unwrap()).unwrap();
    let mut cmd = cli();
    cmd.current_dir(home.path())
        .env("HOME", home.path())
        .env("XDG_CACHE_HOME", cache.parent().unwrap())
        .env("SKILLSMP_API_KEY", "not a valid header");
    let result = tui_request(
        cmd,
        serde_json::json!({"op":"registry", "global":false, "provider":"SkillsMP", "query":" SOME  Query  "}),
    );
    assert!(result.status.success(), "{result:?}");
    let rows: serde_json::Value = serde_json::from_slice(&result.stdout).unwrap();
    assert_eq!(
        rows,
        serde_json::json!([{"ID":"fixture-id","Name":"fixture","Description":"fixture result","Label":"fixture-owner • 42 stars","Provider":"SkillsMP","Locator":"https://github.com/fixture/repo"}])
    );
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

#[cfg(target_os = "linux")]
#[test]
fn inspect_names_candidates_and_acquisition_serves_native_fallback() {
    use sha2::{Digest, Sha256};
    use std::{fs, os::unix::fs::PermissionsExt};

    let home = tempfile::tempdir().unwrap();
    let project = home.path().join("project");
    let local = project.join(".agents/skills/example");
    let bundled = home.path().join("bundled/example");
    let bin = home.path().join("bin");
    for dir in [&local, &bundled, &bin] {
        fs::create_dir_all(dir).unwrap();
    }
    let original = b"---\nname: example\ndescription: Bundled example\n---\nbundled body\n";
    fs::write(bundled.join("SKILL.md"), original).unwrap();
    fs::write(
        local.join("SKILL.md"),
        b"---\nname: example\ndescription: Project example\n---\nproject body\n",
    )
    .unwrap();
    let selection = project.join(".skills-mgr.json");
    fs::write(
        &selection,
        br#"{"schema_revision":3,"skills":{"example":{"enabled":false}}}"#,
    )
    .unwrap();
    let grok = bin.join("grok");
    let cat = serde_json::json!({"skills":[{"name":"example", "description":"Bundled example", "source":{"type":"bundled", "path":bundled.join("SKILL.md")}}]});
    fs::write(&grok, format!("#!/bin/sh\nprintf '%s\\n' '{}'\n", cat)).unwrap();
    fs::set_permissions(grok, fs::Permissions::from_mode(0o755)).unwrap();

    let invoke = |args: &[&str]| {
        cli()
            .args(args)
            .current_dir(&project)
            .env("HOME", home.path())
            .env("XDG_CACHE_HOME", home.path().join("cache"))
            .env("CODEX_HOME", home.path().join("codex"))
            .env("PATH", &bin)
            .env("SKILLS_MGR_NO_REFRESH", "1")
            .output()
            .unwrap()
    };
    let report = invoke(&["inspect", "example"]);
    assert!(report.status.success(), "{report:?}");
    assert!(report.stderr.is_empty());
    let report: serde_json::Value = serde_json::from_slice(&report.stdout).unwrap();
    assert_eq!(report["fallback"], true);
    assert_eq!(report["resolved"]["source"], "grok-bundled");
    assert_eq!(report["resolved"]["scope"], "native");
    assert_eq!(report["resolved"]["selection"]["value"], true);
    assert_eq!(
        report["resolved"]["sha256"],
        Sha256::digest(original)
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>()
    );
    assert_eq!(
        report["candidates"][0]["selection"]["path"],
        selection.to_str().unwrap()
    );
    assert_eq!(report["candidates"][0]["selection"]["value"], false);

    let get = invoke(&["get", "example"]);
    assert!(get.status.success(), "{get:?}");
    assert_eq!(get.stdout, b"bundled body\n");
    assert!(
        get.stderr
            .starts_with(b"skills-mgr: using grok-bundled skill \"example\"")
    );

    use std::{io::Write, process::Stdio};
    let mut child = cli()
        .arg("__tui")
        .current_dir(&project)
        .env("HOME", home.path())
        .env("XDG_CACHE_HOME", home.path().join("cache"))
        .env("CODEX_HOME", home.path().join("codex"))
        .env("PATH", &bin)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    child
        .stdin
        .take()
        .unwrap()
        .write_all(br#"{"op":"snapshot","global":false}"#)
        .unwrap();
    let snapshot = child.wait_with_output().unwrap();
    assert!(snapshot.status.success(), "{snapshot:?}");
    let snapshot: serde_json::Value = serde_json::from_slice(&snapshot.stdout).unwrap();
    let rows = snapshot["Skills"].as_array().unwrap();
    assert_eq!(rows.len(), 2);
    assert_eq!(rows[0]["Source"], "project");
    assert_eq!(rows[1]["Source"], "grok-bundled");
    assert_eq!(rows[1]["ExternalEnabled"], true);
    assert_eq!(snapshot["Selected"]["example"], false);

    let disabled = home.path().join(".grok/config.toml");
    fs::create_dir_all(disabled.parent().unwrap()).unwrap();
    fs::write(disabled, "[skills]\ndisabled = ['example']\n").unwrap();
    let report = invoke(&["inspect", "example"]);
    assert!(report.status.success(), "{report:?}");
    let report: serde_json::Value = serde_json::from_slice(&report.stdout).unwrap();
    assert!(report["resolved"].is_null());
    assert_eq!(report["error"], "skill \"example\" is not enabled");
    assert_eq!(report["candidates"].as_array().unwrap().len(), 2);
    assert_eq!(invoke(&["get", "example"]).status.code(), Some(1));
}
