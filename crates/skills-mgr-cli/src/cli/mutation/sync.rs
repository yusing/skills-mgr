use std::{
    collections::{BTreeMap, BTreeSet},
    ffi::OsString,
    io::Write,
};

use crate::{
    catalog,
    cli::cancel,
    condition::Conditions,
    remote,
    selection::{self, Selection},
};

use super::{
    Result, Scope, SelectionWrite, current_manager, load_global, managed_lock, placeholder,
    selection_lock, store::Store, wants_placeholder,
};

pub(in crate::cli) fn sync(out: &mut impl Write, args: &[OsString]) -> Result<()> {
    if !args.is_empty() {
        return Err("usage: skills-mgr sync".into());
    }
    cancel::install()?;
    let manager = current_manager()?;
    let scope = Scope::new(&manager, false)?;
    let global = load_global(&manager)?.unwrap_or_default();
    let path = scope.selection_dir.join(selection::LOCK_NAME);
    let original = if scope.global {
        global.clone()
    } else {
        selection::load(&path)?
    };
    let mut layer = original.clone();
    let store = Store::new(&manager);
    let mut known = BTreeMap::new();
    for reference in remote::records(&store.root, None)?
        .into_iter()
        .map(|(record, _)| record.reference)
        .chain(global.values().filter_map(|entry| entry.remote.clone()))
    {
        if let Some(previous) = known.insert(reference.name.clone(), reference.clone())
            && previous != reference
        {
            return Err(format!(
                "sync remote skill {:?}: conflicting remote identities",
                reference.name
            )
            .into());
        }
    }
    for (name, reference) in &known {
        if let Some(entry) = layer.get_mut(name) {
            if entry.remote.as_ref().is_some_and(|old| old != reference) {
                return Err(
                    format!("sync remote skill {name:?}: conflicting remote identities").into(),
                );
            }
            entry.remote = Some(reference.clone());
        } else if global
            .get(name)
            .is_some_and(|entry| entry.enabled.is_some())
        {
            layer.insert(
                name.clone(),
                Selection {
                    enabled: None,
                    remote: Some(reference.clone()),
                },
            );
        }
    }
    let mut names = BTreeSet::new();
    for lock in [&layer, &global] {
        names.extend(
            lock.iter()
                .filter_map(|(name, entry)| entry.remote.as_ref().map(|_| name)),
        );
    }
    if names.is_empty() {
        return Ok(());
    }
    let mut plans = Vec::new();
    let mut conditions = Conditions::default();
    for name in names {
        let result = (|| -> Result<()> {
            cancel::check()?;
            let project_entry = layer.get(name);
            let global_entry = global.get(name);
            let global_ref = global_entry.and_then(|entry| entry.remote.as_ref());
            let reference = project_entry
                .and_then(|entry| entry.remote.as_ref())
                .or(global_ref)
                .expect("sync names have a remote reference");
            let project_value = project_entry.and_then(|entry| entry.enabled.as_ref());
            let effective = project_value.or(global_entry.and_then(|entry| entry.enabled.as_ref()));
            let wanted = project_value.is_some_and(wants_placeholder);
            let fetch = global_ref.is_some()
                || scope.global
                || selection::evaluate(&mut conditions, &manager, name, effective, false)?;
            if fetch {
                if !known.contains_key(name)
                    && let Some(owner) = catalog::discover(&manager, &[], Some(name))?.remove(name)
                    && owner.remote_key.as_deref() != Some(&reference.key())
                {
                    return Err(format!(
                        "skill name is already discovered from {}",
                        owner.source.label()
                    )
                    .into());
                }
                let metadata = store.ensure(reference)?;
                if wanted {
                    plans.push(placeholder::Plan::new(
                        scope.placeholder_base,
                        name,
                        &metadata.description,
                        true,
                    )?);
                }
                writeln!(out, "{name}")?;
            }
            if !wanted {
                plans.push(placeholder::Plan::new(
                    scope.placeholder_base,
                    name,
                    "",
                    false,
                )?);
            }
            Ok(())
        })();
        result.map_err(|err| format!("sync remote skill {name:?}: {err}"))?;
    }

    // Network work does not hold selection locks. Commit only if both layers
    // still match what was reconciled and the desired placeholders are safe.
    let _managed = managed_lock(&manager)?;
    let _global = if !scope.global {
        Some(selection_lock(&manager, &manager.managed_home())?)
    } else {
        None
    };
    let mut write = SelectionWrite::open(&manager, &scope)?;
    if load_global(&manager)?.unwrap_or_default() != global
        || (!scope.global && selection::load(&path)? != original)
    {
        return Err("selection changed during sync; retry the operation".into());
    }
    let changed = layer != original;
    write.layer = layer;
    placeholder::publish_all(&plans, || {
        cancel::check()?;
        if changed {
            write.publish(true)?;
        }
        Ok(())
    })
}
