mod dependency;
mod evidence;

use crate::SkillsManager;
use brush_parser::{
    ast,
    word::{self, WordPiece},
};
use dependency::Dependencies;
use evidence::{Evidence, Feature};
use std::io;

#[derive(Default)]
pub struct Conditions {
    evidence: Option<Evidence>,
    dependencies: Option<Dependencies>,
}

impl Conditions {
    pub(super) fn evaluate(
        &mut self,
        manager: &SkillsManager,
        expr: &str,
    ) -> io::Result<Option<bool>> {
        let Ok(program) =
            brush_parser::Parser::new(expr.as_bytes(), &Default::default()).parse_program()
        else {
            return Ok(None);
        };
        let [list] = program.complete_commands.as_slice() else {
            return Ok(None);
        };
        self.list(manager, list, true)
    }

    fn list(
        &mut self,
        manager: &SkillsManager,
        list: &ast::CompoundList,
        run: bool,
    ) -> io::Result<Option<bool>> {
        let [ast::CompoundListItem(list, ast::SeparatorOperator::Sequence)] = list.0.as_slice()
        else {
            return Ok(None);
        };
        let Some(mut value) = self.pipeline(manager, &list.first, run)? else {
            return Ok(None);
        };
        for op in &list.additional {
            let (pipeline, reached) = match op {
                ast::AndOr::And(p) => (p, value),
                ast::AndOr::Or(p) => (p, !value),
            };
            let Some(next) = self.pipeline(manager, pipeline, run && reached)? else {
                return Ok(None);
            };
            if reached {
                value = next;
            }
        }
        Ok(Some(value))
    }

    fn pipeline(
        &mut self,
        manager: &SkillsManager,
        pipeline: &ast::Pipeline,
        run: bool,
    ) -> io::Result<Option<bool>> {
        if pipeline.timed.is_some() {
            return Ok(None);
        }
        // Predicates exchange no bytes. Bash uses the last command's status.
        let mut last = false;
        for cmd in &pipeline.seq {
            let value = match cmd {
                ast::Command::Compound(ast::CompoundCommand::Subshell(s), None) => {
                    self.list(manager, &s.list, run)?
                }
                ast::Command::Simple(s) if s.prefix.is_none() => self.simple(manager, s, run)?,
                _ => None,
            };
            let Some(value) = value else { return Ok(None) };
            last = value;
        }
        Ok(Some(last ^ pipeline.bang))
    }

    fn simple(
        &mut self,
        manager: &SkillsManager,
        s: &ast::SimpleCommand,
        run: bool,
    ) -> io::Result<Option<bool>> {
        let Some(cmd) = &s.word_or_name else {
            return Ok(None);
        };
        let mut words = Vec::new();
        for item in s.suffix.iter().flat_map(|suffix| &suffix.0) {
            let ast::CommandPrefixOrSuffixItem::Word(word) = item else {
                return Ok(None);
            };
            words.push(word.value.as_str());
        }
        if cmd.value == "[" {
            return match words.as_slice() {
                [r#""$PWD""#, "=" | "==", r#""$HOME""#, "]"] => Ok(Some(
                    run && std::path::absolute(&manager.project)?
                        == std::path::absolute(&manager.home)?,
                )),
                _ => Ok(None),
            };
        }
        let Some(cmd) = literal(&cmd.value) else {
            return Ok(None);
        };
        if !matches!(cmd.as_str(), "lang" | "tooling" | "has_dependency") {
            return Ok(None);
        }
        let Some(args) = words.into_iter().map(literal).collect::<Option<Vec<_>>>() else {
            return Ok(None);
        };
        Ok(Some(
            run && self.predicate(
                manager,
                &cmd,
                &args.iter().map(String::as_str).collect::<Vec<_>>(),
            )?,
        ))
    }

    pub fn predicate(
        &mut self,
        manager: &SkillsManager,
        cmd: &str,
        args: &[&str],
    ) -> io::Result<bool> {
        let invalid =
            |msg: &str| io::Error::new(io::ErrorKind::InvalidInput, format!("{cmd}: {msg}"));
        let feature = match (cmd, args) {
            ("lang" | "tooling", [name]) => Some(
                Feature::parse(cmd, name)
                    .ok_or_else(|| invalid(&format!("unsupported name {name:?}")))?,
            ),
            ("has_dependency", [_] | [_, _]) => None,
            _ => return Err(invalid("invalid predicate arguments")),
        };
        if self.evidence.is_none() {
            self.evidence = Some(Evidence::scan(manager)?);
        }
        let evidence = self.evidence.as_ref().unwrap();
        if let Some(feature) = feature {
            return Ok(evidence.features.contains(&feature));
        }
        if self.dependencies.is_none() {
            self.dependencies = Some(Dependencies::load(&evidence.manifests)?);
        }
        self.dependencies
            .as_ref()
            .unwrap()
            .matches(args[0], args.get(1).copied())
    }
}

fn literal(raw: &str) -> Option<String> {
    let mut value = String::new();
    for part in word::parse(raw, &Default::default()).ok()? {
        match part.piece {
            WordPiece::Text(s) if !s.contains(['*', '?', '[', '{', '~']) => value.push_str(&s),
            WordPiece::SingleQuotedText(s) => value.push_str(&s),
            WordPiece::DoubleQuotedSequence(parts) => {
                for p in parts {
                    let WordPiece::Text(s) = p.piece else {
                        return None;
                    };
                    value.push_str(&s);
                }
            }
            _ => return None,
        }
    }
    Some(value)
}
