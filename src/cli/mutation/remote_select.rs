use crate::{
    SkillsManager, catalog,
    cli::cancel,
    remote::RemoteRef,
    selection::{Enabled, Selection},
};

use super::{
    Result, Scope, SelectionWrite, managed_lock, placeholder, store::Store, wants_placeholder,
};

pub(in crate::cli) fn toggle_remote(
    manager: &SkillsManager,
    scope: &Scope<'_>,
    reference: RemoteRef,
) -> Result<bool> {
    if !reference.valid() {
        return Err("invalid remote skill reference".into());
    }
    let name = &reference.name;
    let key = reference.key();
    let _managed = managed_lock(manager)?;
    let mut write = SelectionWrite::open(manager, scope)?;
    let discovered = catalog::discover(manager, &[], Some(name))?.remove(name);
    if let Some(skill) = discovered.as_ref()
        && skill.remote_key.as_deref() != Some(&key)
    {
        return Err(format!(
            "skill name {name:?} is already discovered from {}",
            skill.source.label()
        )
        .into());
    }
    let configured = write
        .layer
        .get(name)
        .and_then(|entry| entry.enabled.as_ref())
        .or_else(|| write.global.as_ref()?.get(name)?.enabled.as_ref());
    let enabled = !discovered.as_ref().is_some_and(|skill| {
        configured.map_or(!scope.global && skill.metadata.hide, wants_placeholder)
    });
    let store = Store::new(manager);
    let metadata = enabled.then(|| store.ensure(&reference)).transpose()?;
    let _store = store.lock()?;
    let record = store
        .find(&reference)?
        .ok_or("persisted remote skill is missing")?;
    if crate::remote::content_root(&store.root, &record.content)?.is_none() {
        return Err(format!("persisted remote skill {name:?} content is missing").into());
    }
    let description = metadata
        .as_ref()
        .map_or("", |metadata| metadata.description.as_str());
    let placeholders = placeholder::Plan::new(scope.placeholder_base, name, description, enabled)?;
    write.layer.insert(
        name.clone(),
        Selection {
            enabled: Some(Enabled::Boolean(enabled)),
            remote: Some(reference),
        },
    );
    placeholders.publish(|| {
        cancel::check()?;
        write.publish(true)
    })?;
    Ok(enabled)
}
