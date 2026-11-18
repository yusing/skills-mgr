use std::{
    ffi::OsString,
    fs,
    io::Write,
    path::{Path, PathBuf},
};

use crate::{
    SkillsManager, catalog,
    catalog::{Skill, Source},
    io_error,
    selection::{LOCK_NAME, Lock, load},
};

use super::{
    Result, Scope, current_manager, load_global, managed_lock, placeholder, same_directory,
    symlink_metadata, wants_placeholder,
};

pub(in crate::cli) fn adopt(out: &mut impl Write, args: &[OsString]) -> Result<()> {
    if !args.is_empty() {
        return Err("usage: skills-mgr adopt".into());
    }
    let manager = current_manager()?;
    let scope = Scope::new(&manager, false)?;
    let _guard = managed_lock(&manager)?;
    let skills = catalog::user_skills(&manager)?;
    if skills.is_empty() {
        return Ok(());
    }
    adopt_skills(&manager, &scope, skills.values(), || {
        for name in skills.keys() {
            writeln!(out, "{name}")?;
        }
        Ok(())
    })
}

pub(in crate::cli) fn relocate(
    manager: &SkillsManager,
    scope: &Scope<'_>,
    name: &str,
    path: &Path,
) -> Result<bool> {
    let _guard = managed_lock(manager)?;
    let skill = catalog::discover(manager, &[], Some(name))?
        .remove(name)
        .ok_or_else(|| format!("skill {name:?} was not discovered"))?;
    if skill.manifest_path() != path {
        return Err(format!("skill {name:?} changed ownership since the catalog was read").into());
    }
    match skill.source {
        Source::User => {
            adopt_skills(manager, scope, [&skill], || Ok(()))?;
            Ok(true)
        }
        Source::Managed => {
            release_skill(manager, &skill)?;
            Ok(false)
        }
        _ => Err("only a user or managed skill can be adopted or released".into()),
    }
}

fn adopt_skills<'a>(
    manager: &SkillsManager,
    scope: &Scope<'_>,
    skills: impl IntoIterator<Item = &'a Skill>,
    done: impl FnOnce() -> Result<()>,
) -> Result<()> {
    let global = load_global(manager)?.unwrap_or_default();
    let project = (!scope.global)
        .then(|| load(&manager.project.join(LOCK_NAME)))
        .transpose()?;
    let dest_root = manager.manager_home().join("skills");
    let moves = skills
        .into_iter()
        .map(|skill| Ok((skill, relocation_paths(skill, &dest_root)?)))
        .collect::<Result<Vec<_>>>()?;
    let mut moved = Vec::new();
    let change = || {
        // Free the user directories first, since they may be the global
        // placeholder destinations after adoption.
        for (_, (source, dest)) in &moves {
            move_directory(source, dest)?;
            moved.push((source, dest));
        }
        let mut plans = Vec::new();
        for (skill, _) in &moves {
            let name = &skill.metadata.name;
            let description = &skill.metadata.description;
            plans.push(placeholder::Plan::new(
                &manager.home,
                name,
                description,
                selected(&global, name),
            )?);
            if let Some(project) = &project {
                plans.push(placeholder::Plan::new(
                    &manager.project,
                    name,
                    description,
                    selected(project, name),
                )?);
            }
        }
        placeholder::publish_all(&plans, done)
    };
    match change() {
        Ok(()) => Ok(()),
        Err(err) => {
            let failures: Vec<_> = moved
                .into_iter()
                .rev()
                .filter_map(|(source, dest)| {
                    move_directory(dest, source)
                        .err()
                        .map(|err| err.to_string())
                })
                .collect();
            if failures.is_empty() {
                Err(err)
            } else {
                Err(format!("{err}; adoption rollback failed: {}", failures.join("; ")).into())
            }
        }
    }
}

fn release_skill(manager: &SkillsManager, skill: &Skill) -> Result<()> {
    let name = &skill.metadata.name;
    let description = &skill.metadata.description;
    let (source, dest) = relocation_paths(skill, &manager.home.join(".agents/skills"))?;
    let mut plans = vec![placeholder::Plan::new(
        &manager.home,
        name,
        description,
        false,
    )?];
    if !same_directory(&manager.project, &manager.home)? {
        plans.push(placeholder::Plan::new(
            &manager.project,
            name,
            description,
            false,
        )?);
    }
    placeholder::publish_all(&plans, || move_directory(&source, &dest))
}

fn selected(lock: &Lock, name: &str) -> bool {
    lock.get(name)
        .and_then(|entry| entry.enabled.as_ref())
        .is_some_and(wants_placeholder)
}

fn relocation_paths(skill: &Skill, dest_root: &Path) -> Result<(PathBuf, PathBuf)> {
    let source = &skill.entry_path;
    check_parent(source.parent().expect("skill entry has a collection"))?;
    let metadata = symlink_metadata(source)?;
    if !metadata.is_some_and(|metadata| metadata.is_dir())
        || source.canonicalize().map_err(|err| io_error(source, err))? != skill.root
    {
        return Err(format!(
            "skill directory {} changed before relocation or contains a symbolic link",
            source.display()
        )
        .into());
    }
    check_parent(dest_root)?;
    Ok((source.clone(), dest_root.join(&skill.metadata.name)))
}

fn check_parent(path: &Path) -> Result<()> {
    if let Some(metadata) = symlink_metadata(path)?
        && !metadata.is_dir()
    {
        return Err(format!(
            "relocation collection {} is not a real directory",
            path.display()
        )
        .into());
    }
    Ok(())
}

fn move_directory(source: &Path, dest: &Path) -> Result<()> {
    let parent = dest.parent().expect("destination has a collection");
    check_parent(parent)?;
    if symlink_metadata(dest)?.is_some() {
        return Err(format!("{} already exists", dest.display()).into());
    }
    fs::create_dir_all(parent).map_err(|err| io_error(parent, err))?;
    check_parent(parent)?;
    fs::rename(source, dest)
        .map_err(|err| format!("move {} to {}: {err}", source.display(), dest.display()).into())
}
