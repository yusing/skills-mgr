//! In-process evaluation of shell-style conditions, not a full shell.
//! Only reached external commands spawn subprocesses.

use std::{
    env, fs, io,
    path::{Component, Path, PathBuf},
    process::{Command, Stdio},
};

use brush_parser::{
    ast::{self, BinaryPredicate as Binary, ExtendedTestExpr, TestExpr, UnaryPredicate as Unary},
    word::{
        self, Parameter, ParameterExpr, ParameterTestType, TildeExpr, WordPiece,
        WordPieceWithSource,
    },
};

/// Additional in-process commands supplied by the embedding application.
pub trait Builtins {
    fn contains(&self, name: &str) -> bool;
    fn run(&mut self, cwd: &Path, name: &str, args: &[String]) -> io::Result<i32>;

    fn variable(&self, name: &str) -> Option<std::ffi::OsString> {
        env::var_os(name)
    }

    /// Admit filesystem probes before the shell interprets their results.
    fn check_read(&self, _path: &Path) -> io::Result<()> {
        Ok(())
    }

    /// Override reached external commands, after expansion and PATH resolution.
    fn external(
        &self,
        _cwd: &Path,
        _program: &Path,
        _args: &[String],
        _capture: bool,
    ) -> Option<io::Result<(i32, Vec<u8>)>> {
        None
    }

    /// Evaluate a host-specific pipeline without moving data or spawning
    /// processes. Return `None` if the pipeline is unsupported.
    fn pipeline(&mut self, _cwd: &Path, _p: &ast::Pipeline) -> io::Result<Option<i32>> {
        Ok(None)
    }
}

impl Builtins for () {
    fn contains(&self, _: &str) -> bool {
        false
    }

    fn run(&mut self, _: &Path, _: &str, _: &[String]) -> io::Result<i32> {
        Err(unsupported("unregistered builtin"))
    }
}

/// Evaluate a trusted expression and return its final status. `cd` is local to
/// the evaluation. Reached executable commands run with the caller's permissions.
/// Use `&mut ()` when no application-specific builtins are needed.
pub fn evaluate(
    expression: &str,
    cwd: &Path,
    home: &Path,
    builtins: &mut dyn Builtins,
) -> io::Result<i32> {
    let program = brush_parser::Parser::new(expression.as_bytes(), &Default::default())
        .parse_program()
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "invalid selection expression"))?;
    let mut evaluator = Evaluator {
        cwd: std::path::absolute(cwd)?,
        home: std::path::absolute(home)?,
        builtins,
    };
    evaluator.program(&program, &mut None)
}

type Output<'a> = Option<&'a mut Vec<u8>>;

struct Evaluator<'a> {
    builtins: &'a mut dyn Builtins,
    cwd: PathBuf,
    home: PathBuf,
}

impl Evaluator<'_> {
    fn program(&mut self, program: &ast::Program, output: &mut Output<'_>) -> io::Result<i32> {
        let mut status = 0;
        for list in &program.complete_commands {
            status = self.list(list, output)?;
        }
        Ok(status)
    }

    fn list(&mut self, list: &ast::CompoundList, output: &mut Output<'_>) -> io::Result<i32> {
        let mut status = 0;
        for ast::CompoundListItem(list, separator) in &list.0 {
            if matches!(separator, ast::SeparatorOperator::Async) {
                return Err(unsupported("background commands"));
            }
            status = self.pipeline(&list.first, output)?;
            for next in &list.additional {
                match next {
                    ast::AndOr::And(pipeline) if status == 0 => {
                        status = self.pipeline(pipeline, output)?;
                    }
                    ast::AndOr::Or(pipeline) if status != 0 => {
                        status = self.pipeline(pipeline, output)?;
                    }
                    _ => {}
                }
            }
        }
        Ok(status)
    }

    fn pipeline(&mut self, p: &ast::Pipeline, output: &mut Output<'_>) -> io::Result<i32> {
        if p.timed.is_some() {
            return Err(unsupported("time"));
        }
        let [command] = p.seq.as_slice() else {
            return self
                .builtins
                .pipeline(&self.cwd, p)?
                .ok_or_else(|| unsupported("pipelines"));
        };
        let status = match command {
            ast::Command::Simple(s) if s.prefix.is_none() => self.simple(s, output)?,
            ast::Command::Compound(ast::CompoundCommand::IfClause(clause), None) => {
                self.if_clause(clause, output)?
            }
            ast::Command::Compound(ast::CompoundCommand::Subshell(s), None) => {
                self.subshell(|shell| shell.list(&s.list, output))?
            }
            ast::Command::Compound(ast::CompoundCommand::BraceGroup(g), None) => {
                self.list(&g.list, output)?
            }
            ast::Command::ExtendedTest(test, None) => exit_status(self.extended(&test.expr)?),
            _ => return Err(unsupported("command syntax")),
        };
        Ok(if p.bang {
            i32::from(status == 0)
        } else {
            status
        })
    }

    fn subshell<T>(&mut self, run: impl FnOnce(&mut Self) -> io::Result<T>) -> io::Result<T> {
        let parent_cwd = self.cwd.clone();
        let result = run(self);
        self.cwd = parent_cwd;
        result
    }

    fn if_clause(
        &mut self,
        clause: &ast::IfClauseCommand,
        output: &mut Output<'_>,
    ) -> io::Result<i32> {
        if self.list(&clause.condition, output)? == 0 {
            return self.list(&clause.then, output);
        }
        for branch in clause.elses.iter().flatten() {
            if branch
                .condition
                .as_ref()
                .map(|list| self.list(list, output))
                .transpose()?
                .unwrap_or(0)
                == 0
            {
                return self.list(&branch.body, output);
            }
        }
        Ok(0)
    }

    fn simple(&mut self, simple: &ast::SimpleCommand, output: &mut Output<'_>) -> io::Result<i32> {
        let Some(name) = &simple.word_or_name else {
            return Err(unsupported("command without a name"));
        };
        let words = simple
            .suffix
            .iter()
            .flat_map(|suffix| &suffix.0)
            .map(|item| match item {
                ast::CommandPrefixOrSuffixItem::Word(word) => Ok(word.value.as_str()),
                _ => Err(unsupported("assignments or redirections")),
            })
            .collect::<io::Result<Vec<_>>>()?;
        let name = self.expand(&name.value, true)?;
        let args = words
            .iter()
            .map(|word| self.expand(word, true))
            .collect::<io::Result<Vec<_>>>()?;
        self.command(&name, &args, output)
    }

    fn command(&mut self, name: &str, args: &[String], output: &mut Output<'_>) -> io::Result<i32> {
        if self.builtins.contains(name) {
            return self.builtins.run(&self.cwd, name, args);
        }
        match name {
            "true" | ":" => Ok(0),
            "false" => Ok(1),
            "test" | "[" => {
                let args = if name == "[" {
                    let Some((last, args)) = args.split_last() else {
                        return Ok(2);
                    };
                    if last != "]" {
                        return Ok(2);
                    }
                    args
                } else {
                    args
                };
                let Ok(test) = brush_parser::test_command::parse(args) else {
                    return Ok(2);
                };
                self.test(&test).map(exit_status)
            }
            "command" => match args {
                [flag, target] if flag == "-v" => {
                    if builtin(target) || self.builtins.contains(target) {
                        write_output(output, format!("{target}\n").as_bytes());
                    } else if let Some(path) = self.executable(target)? {
                        write_output(output, format!("{}\n", path.display()).as_bytes());
                    } else {
                        return Ok(1);
                    }
                    Ok(0)
                }
                _ => Err(unsupported("command options (expected command -v NAME)")),
            },
            "cd" => self.cd(args),
            "pwd" if args.is_empty() => {
                write_output(output, format!("{}\n", self.cwd.display()).as_bytes());
                Ok(0)
            }
            "echo" if !args.iter().any(|arg| matches!(arg.as_str(), "-e" | "-E")) => {
                let (args, newline) = match args.split_first() {
                    Some((flag, rest)) if flag == "-n" => (rest, false),
                    _ => (args, true),
                };
                write_output(output, args.join(" ").as_bytes());
                if newline {
                    write_output(output, b"\n");
                }
                Ok(0)
            }
            name if builtin(name) => Err(unsupported(&format!("the {name} builtin"))),
            name => self.external(name, args, output),
        }
    }

    fn cd(&mut self, args: &[String]) -> io::Result<i32> {
        let (args, literal) = match args {
            [flag, rest @ ..] if flag == "--" => (rest, true),
            _ => (args, false),
        };
        let target = match args {
            [] => self.home.clone(),
            [dir] if dir.is_empty() => return Ok(1),
            [dir] if !literal && dir.starts_with('-') => return Err(unsupported("cd options")),
            [dir] => self.cwd.join(dir),
            _ => return Ok(1),
        };
        let mut cwd = PathBuf::new();
        // Resolve ".." against the expression's logical path, not the target of
        // a symlink. A component must still exist and be searchable to enter it.
        for part in target.components() {
            if part == Component::ParentDir {
                self.builtins.check_read(&cwd)?;
                if !cwd.join(".").is_dir() {
                    return Ok(1);
                }
                cwd.pop();
            } else {
                cwd.push(part);
            }
        }
        self.builtins.check_read(&cwd)?;
        if !cwd.join(".").is_dir() {
            return Ok(1);
        }
        self.cwd = cwd;
        Ok(0)
    }

    fn external(&self, name: &str, args: &[String], output: &mut Output<'_>) -> io::Result<i32> {
        let Some(executable) = self.executable(name)? else {
            return Ok(127);
        };
        if let Some(outcome) =
            self.builtins
                .external(&self.cwd, &executable, args, output.is_some())
        {
            let (status, bytes) = outcome?;
            write_output(output, &bytes);
            return Ok(status);
        }
        let mut command = Command::new(executable);
        command
            .args(args)
            .current_dir(&self.cwd)
            .env("PWD", &self.cwd)
            .env("HOME", &self.home)
            .stdin(Stdio::null())
            .stderr(Stdio::null());
        let status = if let Some(buffer) = output {
            let child = command.output()?;
            buffer.extend(child.stdout);
            child.status
        } else {
            command.stdout(Stdio::null()).status()?
        };
        Ok(status.code().unwrap_or_else(|| {
            #[cfg(unix)]
            {
                use std::os::unix::process::ExitStatusExt;
                128 + status.signal().unwrap_or(0)
            }
            #[cfg(not(unix))]
            {
                128
            }
        }))
    }

    fn executable(&self, name: &str) -> io::Result<Option<PathBuf>> {
        if name.is_empty() {
            return Ok(None);
        }
        if name.contains(std::path::MAIN_SEPARATOR) || name.contains('/') {
            let path = self.cwd.join(name);
            self.builtins.check_read(&path)?;
            return Ok(is_executable(&path).then_some(path));
        }
        if let Some(paths) = self.builtins.variable("PATH") {
            for dir in env::split_paths(&paths) {
                let path = self.cwd.join(dir).join(name);
                self.builtins.check_read(&path)?;
                if is_executable(&path) {
                    return Ok(Some(path));
                }
            }
        }
        Ok(None)
    }

    fn expand(&mut self, raw: &str, command_word: bool) -> io::Result<String> {
        let parts =
            word::parse(raw, &Default::default()).map_err(|_| unsupported("word syntax"))?;
        self.pieces(&parts, command_word)
    }

    fn pieces(&mut self, parts: &[WordPieceWithSource], command_word: bool) -> io::Result<String> {
        let mut result = String::new();
        for part in parts {
            match &part.piece {
                WordPiece::Text(text)
                    if !command_word
                        || !text.contains(['*', '?', '[', '{', '~'])
                        || text == "[" =>
                {
                    result.push_str(text)
                }
                WordPiece::SingleQuotedText(text) => result.push_str(text),
                WordPiece::EscapeSequence(text) => {
                    result.push_str(text.strip_prefix('\\').unwrap_or(text))
                }
                WordPiece::DoubleQuotedSequence(parts) => {
                    result.push_str(&self.pieces(parts, false)?)
                }
                WordPiece::TildeExpansion(TildeExpr::Home) => {
                    result.push_str(&self.home.to_string_lossy());
                }
                WordPiece::ParameterExpansion(ParameterExpr::Parameter {
                    parameter: Parameter::Named(name),
                    indirect: false,
                }) => {
                    let value = self.variable(name).unwrap_or_default();
                    push_expansion(&mut result, &value, command_word)?;
                }
                WordPiece::ParameterExpansion(ParameterExpr::UseDefaultValues {
                    parameter: Parameter::Named(name),
                    indirect: false,
                    test_type,
                    default_value,
                }) => {
                    let value = match self.variable(name) {
                        Some(value)
                            if !value.is_empty()
                                || matches!(test_type, ParameterTestType::Unset) =>
                        {
                            value
                        }
                        _ => self.expand(default_value.as_deref().unwrap_or_default(), false)?,
                    };
                    push_expansion(&mut result, &value, command_word)?;
                }
                WordPiece::CommandSubstitution(script)
                | WordPiece::BackquotedCommandSubstitution(script) => {
                    let program = brush_parser::Parser::new(script.as_bytes(), &Default::default())
                        .parse_program()
                        .map_err(|_| unsupported("invalid command substitution"))?;
                    let mut bytes = Vec::new();
                    self.subshell(|shell| shell.program(&program, &mut Some(&mut bytes)))?;
                    bytes.retain(|byte| *byte != 0);
                    let value = String::from_utf8_lossy(&bytes);
                    push_expansion(&mut result, value.trim_end_matches('\n'), command_word)?;
                }
                _ => return Err(unsupported("word expansion")),
            }
        }
        Ok(result)
    }

    fn variable(&self, name: &str) -> Option<String> {
        match name {
            "PWD" => Some(self.cwd.to_string_lossy().into_owned()),
            "HOME" => Some(self.home.to_string_lossy().into_owned()),
            _ => self
                .builtins
                .variable(name)
                .and_then(|s| s.into_string().ok()),
        }
    }

    fn extended(&mut self, expr: &ExtendedTestExpr) -> io::Result<bool> {
        Ok(match expr {
            ExtendedTestExpr::And(a, b) => self.extended(a)? && self.extended(b)?,
            ExtendedTestExpr::Or(a, b) => self.extended(a)? || self.extended(b)?,
            ExtendedTestExpr::Not(expr) => !self.extended(expr)?,
            ExtendedTestExpr::Parenthesized(expr) => self.extended(expr)?,
            ExtendedTestExpr::UnaryTest(op, word) => {
                let value = self.expand(&word.value, false)?;
                self.unary(op, &value)?
            }
            ExtendedTestExpr::BinaryTest(op, a, b) => {
                let left = self.expand(&a.value, false)?;
                let right = self.expand(&b.value, false)?;
                if matches!(
                    op,
                    Binary::StringExactlyMatchesPattern | Binary::StringDoesNotExactlyMatchPattern
                ) && right.contains(['*', '?', '['])
                    && !matches!(word::parse(&b.value, &Default::default()).ok().as_deref(), Some([part])
                        if matches!(part.piece, WordPiece::SingleQuotedText(_) | WordPiece::DoubleQuotedSequence(_)))
                {
                    return Err(unsupported("wildcards in [[ tests ]]"));
                }
                binary(op, &left, &right)?
            }
        })
    }

    fn test(&self, expr: &TestExpr) -> io::Result<bool> {
        Ok(match expr {
            TestExpr::False => false,
            TestExpr::Literal(value) => !value.is_empty(),
            TestExpr::And(a, b) => self.test(a)? && self.test(b)?,
            TestExpr::Or(a, b) => self.test(a)? || self.test(b)?,
            TestExpr::Not(expr) => !self.test(expr)?,
            TestExpr::Parenthesized(expr) => self.test(expr)?,
            TestExpr::UnaryTest(op, value) => self.unary(op, value)?,
            TestExpr::BinaryTest(op, a, b) => binary(op, a, b)?,
        })
    }

    fn unary(&self, op: &Unary, value: &str) -> io::Result<bool> {
        let path = || self.cwd.join(value);
        if !value.is_empty()
            && !matches!(
                op,
                Unary::StringHasZeroLength | Unary::StringHasNonZeroLength
            )
        {
            self.builtins.check_read(&path())?;
        }
        Ok(match op {
            Unary::FileExists => !value.is_empty() && path().exists(),
            Unary::FileExistsAndIsRegularFile => path().is_file(),
            Unary::FileExistsAndIsDir => !value.is_empty() && path().is_dir(),
            Unary::FileExistsAndIsSymlink => path().is_symlink(),
            Unary::FileExistsAndIsNotZeroLength => {
                !value.is_empty() && fs::metadata(path()).is_ok_and(|meta| meta.len() != 0)
            }
            Unary::FileExistsAndIsExecutable => is_executable(&path()),
            Unary::StringHasZeroLength => value.is_empty(),
            Unary::StringHasNonZeroLength => !value.is_empty(),
            op => return Err(unsupported(&format!("test operator {op}"))),
        })
    }
}

fn binary(op: &Binary, a: &str, b: &str) -> io::Result<bool> {
    Ok(match op {
        Binary::StringExactlyMatchesString | Binary::StringExactlyMatchesPattern => a == b,
        Binary::StringDoesNotExactlyMatchString | Binary::StringDoesNotExactlyMatchPattern => {
            a != b
        }
        Binary::LeftSortsBeforeRight => a < b,
        Binary::LeftSortsAfterRight => a > b,
        Binary::ArithmeticEqualTo
        | Binary::ArithmeticNotEqualTo
        | Binary::ArithmeticLessThan
        | Binary::ArithmeticLessThanOrEqualTo
        | Binary::ArithmeticGreaterThan
        | Binary::ArithmeticGreaterThanOrEqualTo => {
            let integer = |value: &str| {
                value.parse::<i64>().map_err(|_| {
                    io::Error::new(
                        io::ErrorKind::InvalidInput,
                        "expected a signed decimal integer",
                    )
                })
            };
            let (a, b) = (integer(a)?, integer(b)?);
            match op {
                Binary::ArithmeticEqualTo => a == b,
                Binary::ArithmeticNotEqualTo => a != b,
                Binary::ArithmeticLessThan => a < b,
                Binary::ArithmeticLessThanOrEqualTo => a <= b,
                Binary::ArithmeticGreaterThan => a > b,
                _ => a >= b,
            }
        }
        op => return Err(unsupported(&format!("test operator {op}"))),
    })
}

fn is_executable(path: &Path) -> bool {
    let Ok(metadata) = fs::metadata(path) else {
        return false;
    };
    if !metadata.is_file() {
        return false;
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        metadata.permissions().mode() & 0o111 != 0
    }
    #[cfg(not(unix))]
    {
        true
    }
}

fn builtin(name: &str) -> bool {
    // Do not silently run an executable with a builtin's name when its builtin
    // semantics are unavailable in a condition.
    matches!(
        name,
        ":" | "."
            | "["
            | "alias"
            | "bg"
            | "bind"
            | "break"
            | "builtin"
            | "caller"
            | "cd"
            | "command"
            | "compgen"
            | "complete"
            | "compopt"
            | "continue"
            | "declare"
            | "dirs"
            | "disown"
            | "echo"
            | "enable"
            | "eval"
            | "exec"
            | "exit"
            | "export"
            | "false"
            | "fc"
            | "fg"
            | "getopts"
            | "hash"
            | "help"
            | "history"
            | "jobs"
            | "kill"
            | "let"
            | "local"
            | "logout"
            | "mapfile"
            | "popd"
            | "printf"
            | "pushd"
            | "pwd"
            | "read"
            | "readarray"
            | "readonly"
            | "return"
            | "set"
            | "shift"
            | "shopt"
            | "source"
            | "suspend"
            | "test"
            | "times"
            | "trap"
            | "true"
            | "type"
            | "typeset"
            | "ulimit"
            | "umask"
            | "unalias"
            | "unset"
            | "wait"
    )
}

fn push_expansion(result: &mut String, value: &str, command_word: bool) -> io::Result<()> {
    if command_word && (value.is_empty() || value.contains([' ', '\t', '\n', '*', '?', '['])) {
        return Err(unsupported(
            "unquoted expansion requiring word splitting or globbing",
        ));
    }
    result.push_str(value);
    Ok(())
}

fn write_output(output: &mut Output<'_>, bytes: &[u8]) {
    if let Some(buffer) = output {
        buffer.extend_from_slice(bytes);
    }
}

fn exit_status(enabled: bool) -> i32 {
    i32::from(!enabled)
}

fn unsupported(syntax: &str) -> io::Error {
    io::Error::new(
        io::ErrorKind::Unsupported,
        format!("unsupported selection expression: {syntax}"),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn returns_shell_status_without_application_builtins() {
        let cwd = env::current_dir().unwrap();
        assert_eq!(evaluate("test 10 -lt 2", &cwd, &cwd, &mut ()).unwrap(), 1);
        assert_eq!(
            evaluate("cd && [[ $PWD == ~ ]]", &cwd, &cwd, &mut ()).unwrap(),
            0
        );
    }
}
