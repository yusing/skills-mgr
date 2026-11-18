use std::{io, path::Path};

use brush_parser::ast;
use skills_mgr_shell::Builtins;

use super::{Conditions, is_project_predicate};
use crate::SkillsManager;

pub(super) fn evaluate(
    conditions: &mut Conditions,
    manager: &SkillsManager,
    expression: &str,
) -> io::Result<bool> {
    let cwd = std::path::absolute(&manager.project)?;
    let mut builtins = ProjectPredicates {
        initial_cwd: &cwd,
        conditions,
        manager,
        local: None,
    };
    match skills_mgr_shell::evaluate(expression, &cwd, &manager.home, &mut builtins)? {
        0 => Ok(true),
        1 => Ok(false),
        status => Err(io::Error::other(format!("status {status}"))),
    }
}

struct ProjectPredicates<'a> {
    initial_cwd: &'a Path,
    conditions: &'a mut Conditions,
    manager: &'a SkillsManager,
    local: Option<(SkillsManager, Conditions)>,
}

impl ProjectPredicates<'_> {
    fn scope(&mut self, cwd: &Path) -> (&mut Conditions, &SkillsManager) {
        if cwd == self.initial_cwd {
            return (self.conditions, self.manager);
        }
        let (manager, conditions) = self.local.get_or_insert_with(|| {
            (
                SkillsManager::new(cwd, &self.manager.home, &self.manager.cache),
                Conditions::default(),
            )
        });
        if manager.project != cwd {
            cwd.clone_into(&mut manager.project);
            *conditions = Conditions::default();
        }
        (conditions, manager)
    }
}

impl Builtins for ProjectPredicates<'_> {
    fn contains(&self, name: &str) -> bool {
        is_project_predicate(name)
    }

    fn run(&mut self, cwd: &Path, name: &str, args: &[String]) -> io::Result<i32> {
        let (conditions, manager) = self.scope(cwd);
        let args: Vec<_> = args.iter().map(String::as_str).collect();
        conditions
            .predicate(manager, name, &args)
            .map(|enabled| i32::from(!enabled))
    }

    fn pipeline(&mut self, cwd: &Path, pipeline: &ast::Pipeline) -> io::Result<Option<i32>> {
        let (conditions, manager) = self.scope(cwd);
        // Native predicates exchange no bytes. Check the whole pipeline before
        // evaluating any of them, and keep the last predicate's status.
        if conditions.pipeline(manager, pipeline, false)?.is_none() {
            return Ok(None);
        }
        conditions
            .pipeline(manager, pipeline, true)
            .map(|value| value.map(|enabled| i32::from(!enabled)))
    }
}
