use std::{
    ffi::OsString,
    io::{self, Write},
};

use crate::{
    catalog,
    cli::cancel,
    content::valid_name,
    selection::{Enabled, Selection},
};

use super::{
    Result, Scope, SelectionWrite, current_manager, exclusion::Exclusion, managed_lock,
    placeholder, repository, store::Store,
};

pub(in crate::cli) fn install(out: &mut impl Write, mut args: &[OsString]) -> Result<()> {
    let usage =
        "usage: skills-mgr install [-g] [--exclude <pattern>]... <repo-address> [skill-name]";
    let mut force_global = false;
    let mut patterns = Vec::new();
    while let [head, rest @ ..] = args {
        match head.to_str() {
            Some("-g") => {
                force_global = true;
                args = rest;
            }
            Some("--exclude") => {
                let [pattern, tail @ ..] = rest else {
                    return Err(usage.into());
                };
                patterns.push(pattern.to_str().ok_or(usage)?.to_owned());
                args = tail;
            }
            Some(head) if head.starts_with('-') => return Err(usage.into()),
            _ => break,
        }
    }
    let (address, name) = match args {
        [address] => (address, None),
        [address, name] => (
            address,
            Some(
                name.to_str()
                    .filter(|name| valid_name(name))
                    .ok_or("invalid skill name")?,
            ),
        ),
        _ => return Err(usage.into()),
    };
    let address = address.to_str().ok_or(usage)?;
    let exclusion = Exclusion::new(patterns)?;
    let manager = current_manager()?;
    let scope = Scope::new(&manager, force_global)?;
    cancel::install()?;
    let mut diagnostics = io::stderr().lock();
    let (reference, files) =
        repository::install_files(address, name, &exclusion, &mut diagnostics)?;
    writeln!(diagnostics, "Installing and enabling {}...", reference.name)?;
    let name = reference.name.clone();
    let store = Store::new(&manager);
    let prepared = store.prepare(reference.clone(), files, exclusion.patterns.clone())?;
    let _managed = managed_lock(&manager)?;
    let mut selection = SelectionWrite::open(&manager, &scope)?;
    let _store = store.lock()?;
    let current = store.find(&reference)?;
    let key = reference.key();
    if let Some(existing) = catalog::discover(&manager, &[], Some(&name))?.remove(&name)
        && existing.remote_key.as_ref() != Some(&key)
    {
        return Err(format!(
            "skill name {name:?} is already discovered from {}",
            existing.source.label()
        )
        .into());
    }
    let placeholder = placeholder::Plan::new(
        scope.placeholder_base,
        &name,
        &prepared.metadata.description,
        true,
    )?;
    selection.layer.insert(
        name.clone(),
        Selection {
            enabled: Some(Enabled::Boolean(true)),
            remote: Some(reference),
        },
    );
    placeholder.publish(|| {
        store.publish(prepared, current.is_none(), Some(&exclusion), || {
            selection.publish(true)
        })
    })?;
    let layer = if scope.global { "global" } else { "project" };
    writeln!(out, "Installed {name} ({layer} selection)")?;
    Ok(())
}
