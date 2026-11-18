use std::{collections::BTreeMap, io::Write};

use crate::{
    SkillsManager,
    catalog::{self, Skill, Source},
    cli::cancel,
    remote,
    selection::{self, Lock},
};

use super::{
    Result, Scope, load_global, managed_lock, placeholder, selection_lock, store::Store,
    wants_placeholder,
};

pub(in crate::cli) fn repair_managed(manager: &SkillsManager, log: &mut impl Write) {
    let result = (|| {
        let _managed = managed_lock(manager)?;
        let managed = catalog::managed_skills(manager)?;
        if managed.is_empty() {
            return Ok(());
        }
        let global_scope = Scope::new(manager, true)?;
        let global = {
            let _global = selection_lock(manager, &global_scope.selection_dir)?;
            let global = load_global(manager)?.unwrap_or_default();
            if let Err(error) = repair_scope(managed, &global_scope, &global, &global) {
                let _ = writeln!(log, "repair global managed placeholders: {error}");
            }
            global
        };
        let scope = Scope::new(manager, false)?;
        if !scope.global {
            let _project = selection_lock(manager, &scope.selection_dir)?;
            let project = selection::load(&scope.selection_dir.join(selection::LOCK_NAME))?;
            let installed = catalog::discover(manager, &[], None)?;
            repair_scope(installed, &scope, &project, &global)?;
        }
        Ok::<_, Box<dyn std::error::Error>>(())
    })();
    if let Err(error) = result {
        let _ = writeln!(log, "repair managed placeholders: {error}");
    }
}

fn repair_scope(
    skills: BTreeMap<String, Skill>,
    scope: &Scope<'_>,
    layer: &Lock,
    global: &Lock,
) -> Result<()> {
    let mut plans = Vec::new();
    for (name, skill) in skills {
        cancel::check()?;
        if !matches!(skill.source, Source::Managed)
            || [layer, global]
                .iter()
                .any(|lock| lock.get(&name).is_some_and(|entry| entry.remote.is_some()))
        {
            continue;
        }
        let enabled = layer
            .get(&name)
            .and_then(|entry| entry.enabled.as_ref())
            .is_some_and(wants_placeholder);
        plans.push(placeholder::Plan::new(
            scope.placeholder_base,
            &name,
            &skill.metadata.description,
            enabled,
        )?);
    }
    placeholder::publish_all(&plans, || Ok(cancel::check()?))
}

pub(in crate::cli) fn refresh_remotes(manager: &SkillsManager, log: &mut impl Write) -> Result<()> {
    let store = Store::new(manager);
    for (record, _) in remote::records(&store.root, None)? {
        cancel::check()?;
        if let Err(error) = store.refresh(&record.reference) {
            writeln!(
                log,
                "refresh remote skill {:?}: {error}",
                record.reference.name
            )?;
        }
    }
    store.cleanup()
}
