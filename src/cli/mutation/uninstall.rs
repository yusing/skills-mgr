use std::{ffi::OsString, io::Write};

use crate::{SkillsManager, catalog, cli::cancel, content::valid_name, remote, selection};

use super::{
    Result, Scope, SelectionWrite, current_manager, global_flag, managed_lock, placeholder,
    selection_lock, store::Store,
};

pub(in crate::cli) fn uninstall(out: &mut impl Write, args: &[OsString]) -> Result<()> {
    let usage = "usage: skills-mgr uninstall [-g] <skill>";
    let (force_global, args) = global_flag(args);
    let [name] = args else {
        return Err(usage.into());
    };
    let name = name.to_str().ok_or(usage)?;
    cancel::install()?;
    let manager = current_manager()?;
    let scope = Scope::new(&manager, force_global)?;
    uninstall_skill(&manager, &scope, name, None)?;
    writeln!(out, "Uninstalled {name}")?;
    Ok(())
}

pub(in crate::cli) fn uninstall_skill(
    manager: &SkillsManager,
    scope: &Scope<'_>,
    name: &str,
    expected: Option<&str>,
) -> Result<()> {
    if !valid_name(name) {
        return Err(format!("invalid skill name {name:?}").into());
    }
    let _managed = managed_lock(manager)?;
    let _global = if scope.global {
        None
    } else {
        let guard = selection_lock(manager, &manager.manager_home())?;
        if let Some(path) = selection::global_path(manager)? {
            let (revision, global) = selection::load_with_revision(&path)?;
            if global.get(name).is_some_and(|entry| {
                entry.remote.is_some() || revision == 1 && entry.enabled.is_some()
            }) {
                return Err(format!("remote skill {name:?} is configured globally; use skills-mgr uninstall -g {name}").into());
            }
        }
        Some(guard)
    };
    let mut selection = SelectionWrite::open(manager, scope)?;
    let store = Store::new(manager);
    let _store = store.lock()?;
    let mut records = remote::records(&store.root, Some(name))?;
    let record = records
        .pop()
        .filter(|(_, key)| expected.is_none_or(|value| key == value));
    let Some((record, _)) = record else {
        return Err(format!("skill {name:?} is not an installed remote skill; local and harness-owned skills cannot be uninstalled").into());
    };
    if !records.is_empty() {
        return Err(format!("multiple persisted remote identities for skill {name:?}").into());
    }
    if expected.is_none() && catalog::discover_local(manager, name)? {
        return Err(format!(
            "skill {name:?} has a local or harness-owned copy; it cannot be uninstalled by name"
        )
        .into());
    }
    if selection
        .layer
        .get(name)
        .and_then(|entry| entry.remote.as_ref())
        .is_some_and(|reference| *reference != record.reference)
    {
        return Err(format!(
            "selection and persisted remote skill {name:?} have different identities"
        )
        .into());
    }
    // This also checks retained cache paths without requiring the content to exist.
    remote::content_root(&store.root, &record.content)?;
    let placeholders = placeholder::Plan::new(scope.placeholder_base, name, "", false)?;
    let changed = selection.layer.remove(name).is_some();
    placeholders.publish(|| {
        store.remove(&record, || {
            cancel::check()?;
            selection.publish(changed)
        })
    })
}
