use std::{
    ffi::OsString,
    io::{Read, Write},
    path::{Path, PathBuf},
};

use serde::Serialize;
use sha2::{Digest, Sha256};

use crate::{
    Error, SkillsManager,
    catalog::{self, Skill, Source},
    condition::Conditions,
    content, io_error, remote,
    selection::{self, Enabled, LOCK_NAME},
};

use super::{Result, current_manager, native};

#[derive(Serialize)]
struct Report<'a> {
    name: &'a str,
    project: &'a Path,
    resolved: Option<Owner<'a>>,
    fallback: bool,
    candidates: Vec<Owner<'a>>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    warnings: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
}

#[derive(Serialize)]
struct Owner<'a> {
    source: &'static str,
    scope: &'static str,
    root: &'a Path,
    path: PathBuf,
    #[serde(skip_serializing_if = "str::is_empty")]
    plugin: &'a str,
    disable_model_invocation: bool,
    editable: bool,
    enabled: bool,
    selection: Layer<'a>,
    #[serde(skip_serializing_if = "Option::is_none")]
    remote_key: Option<&'a str>,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    content_missing: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    patch_path: Option<PathBuf>,
    #[serde(skip_serializing_if = "Option::is_none")]
    sha256: Option<String>,
    body_health: &'static str,
    references: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
}

#[derive(Serialize)]
struct Layer<'a> {
    layer: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    path: Option<&'a Path>,
    value: &'a Enabled,
}

pub(super) fn inspect(out: &mut impl Write, args: &[OsString]) -> Result<()> {
    let [name] = args else {
        return Err("usage: skills-mgr inspect <skill>".into());
    };
    let name = name.to_string_lossy();
    if !content::valid_name(&name) {
        return Err(format!("invalid skill name {name:?}").into());
    }
    let manager = current_manager()?;
    let report = |resolved, candidates, fallback, warnings, error| Report {
        name: &name,
        project: &manager.project,
        resolved,
        fallback,
        candidates,
        warnings,
        error,
    };
    let mut files = catalog::discover(&manager, &[], Some(&name))?
        .into_values()
        .map(|skill| (skill, false))
        .collect::<Vec<_>>();
    if files.is_empty() {
        files.extend(
            native::missing_remote(&manager, Some(&name))?
                .into_iter()
                .map(|skill| (skill, true)),
        );
    }
    let mut warnings = Vec::new();
    for result in [
        catalog::claude_plugins(&manager, Some(&name), true).map_err(Into::into),
        native::grok(&manager, Some(&name)),
    ] {
        match result {
            Ok(skills) => files.extend(skills.into_iter().map(|skill| (skill, false))),
            Err(e) => warnings.push(e.to_string()),
        }
    }
    let project_path = manager.project.join(LOCK_NAME);
    let has_shared = files.iter().any(|(skill, _)| skill.native.is_none());
    let project = has_shared
        .then(|| selection::load(&project_path))
        .transpose()?
        .unwrap_or_default();
    let global_path = if has_shared {
        selection::global_path(&manager)?
    } else {
        None
    };
    let global = global_path
        .as_deref()
        .map(selection::load)
        .transpose()?
        .unwrap_or_default();
    let explicit = [
        (
            "project",
            Some(project_path.as_path()),
            project.get(name.as_ref()),
        ),
        ("global", global_path.as_deref(), global.get(name.as_ref())),
    ]
    .into_iter()
    .find_map(|(layer, path, entry)| {
        Some(Layer {
            layer,
            path,
            value: entry?.enabled.as_ref()?,
        })
    });
    let enabled = Enabled::Boolean(true);
    let disabled = Enabled::Boolean(false);
    let flag = |value| if value { &enabled } else { &disabled };
    let mut resolved = None;
    let mut candidates = Vec::new();
    let mut fallback = false;
    let mut access_error = None;
    let mut evaluator = Conditions::default();
    for (skill, missing) in &files {
        let selection = if let Some(native) = &skill.native {
            Layer {
                layer: "native",
                path: None,
                value: flag(native.enabled),
            }
        } else if let Some(layer) = &explicit {
            Layer {
                layer: layer.layer,
                path: layer.path,
                value: layer.value,
            }
        } else {
            Layer {
                layer: "default",
                path: None,
                value: flag(skill.source.is_project() || skill.metadata.hide),
            }
        };
        let (enabled, mut error) = match selection::evaluate(
            &mut evaluator,
            &manager,
            &name,
            Some(selection.value),
            false,
        ) {
            Ok(enabled) => (enabled, None),
            Err(e) => {
                let error = e.to_string();
                if resolved.is_none() && access_error.is_none() && !missing {
                    access_error = Some(error.clone());
                }
                (false, Some(error))
            }
        };
        let (sha256, body) = match full_manifest(&manager, skill) {
            Ok(bytes) => (
                Some(remote::hex_digest(&Sha256::digest(&bytes))),
                content::validate_body(&bytes, true),
            ),
            Err(e) => (None, Err(e.to_string())),
        };
        let body_health = if let Err(body_error) = body {
            error =
                Some(error.map_or_else(|| body_error.clone(), |e| format!("{e}\n{body_error}")));
            "error"
        } else {
            "ok"
        };
        let mut references = match content::open_root(&skill.root)
            .and_then(|dir| catalog::references(&skill.root, &dir))
        {
            Ok(references) => references,
            Err(e) => {
                if !missing {
                    warnings.push(e.to_string());
                }
                Vec::new()
            }
        };
        references.sort_by_key(|path| path.contains('/'));
        let scope = if skill.native.is_some() {
            "native"
        } else if skill.source.is_project()
            || (matches!(
                skill.source,
                Source::Native(_)
                    | Source::CodexDirectory
                    | Source::Bundled
                    | Source::Admin
                    | Source::CodexPlugin
            ) && !at_home(&manager)
                && skill.root.starts_with(&manager.project))
        {
            "project"
        } else {
            "shared"
        };
        let owner = Owner {
            source: skill.source.label(),
            scope,
            root: &skill.root,
            path: skill.manifest_path(),
            plugin: skill
                .native
                .as_ref()
                .map_or("", |native| native.plugin.as_str()),
            disable_model_invocation: skill.metadata.hide,
            editable: skill.source.editable(),
            enabled,
            selection,
            remote_key: skill.remote_key.as_deref(),
            content_missing: *missing,
            patch_path: skill
                .remote_key
                .as_ref()
                .map(|key| remote::patch_path(&manager, key)),
            sha256,
            body_health,
            references,
            error,
        };
        if resolved.is_none() && access_error.is_none() && enabled && !missing {
            fallback = skill.native.is_some() && !candidates.is_empty();
            resolved = Some(owner);
        } else {
            candidates.push(owner);
        }
    }
    let error = resolved.is_none().then(|| {
        access_error.clone().unwrap_or_else(|| match files.first() {
            Some((_, true)) => {
                format!("skill {name:?} has missing cached content; run skills-mgr sync")
            }
            Some(_) => Error::NotEnabled(name.to_string()).to_string(),
            None => warnings
                .first()
                .cloned()
                .unwrap_or_else(|| Error::NotFound(name.to_string()).to_string()),
        })
    });
    let status_error = if files.is_empty() {
        error.clone()
    } else {
        access_error
    };
    serde_json::to_writer_pretty(
        &mut *out,
        &report(resolved, candidates, fallback, warnings, error),
    )?;
    writeln!(out)?;
    if let Some(error) = status_error {
        return Err(error.into());
    }
    Ok(())
}

pub(super) fn at_home(manager: &SkillsManager) -> bool {
    let same = |path: &Path| {
        path == manager.project
            || path
                .canonicalize()
                .is_ok_and(|path| path == manager.project)
    };
    same(&manager.home) || same(&manager.manager_home())
}

pub(super) fn full_manifest(manager: &SkillsManager, skill: &Skill) -> Result<Vec<u8>> {
    let dir = content::open_root(&skill.root)?;
    let path = skill.manifest_path();
    let mut contents = Vec::new();
    content::open_file(&dir, path.strip_prefix(&skill.root)?)
        .and_then(|mut file| file.read_to_end(&mut contents))
        .map_err(|e| io_error(&path, e))?;
    if let Some(key) = &skill.remote_key
        && let Some(patch) = remote::patch(manager, key)?
    {
        return Ok(patch.apply(&contents)?);
    }
    Ok(contents)
}
