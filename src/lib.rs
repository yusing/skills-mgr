//! Read an existing skills-mgr selection and catalog in process.
//!
//! Structured reads do not spawn processes. The XML catalog evaluates selections
//! in-process and may run external commands explicitly invoked by a condition.
//! The library does not fetch skills or change selection and cache files itself.

mod catalog;
mod condition;
mod content;
mod remote;
mod selection;

#[cfg(feature = "cli-support")]
#[doc(hidden)]
pub mod cli_support;

use std::{
    env, io,
    ops::RangeInclusive,
    path::{Path, PathBuf},
};

/// An agent whose native catalog should be omitted from the advertised list.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Harness {
    Claude,
    Codex,
    Grok,
}

/// An advertised skill. Reference paths are relative to the skill directory and
/// can be appended to its name to form a get target.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ListedSkill {
    pub name: String,
    pub description: String,
    pub references: Vec<String>,
}

/// Failures from the read API.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum Error {
    #[error("cannot locate the user's {0} directory")]
    MissingDirectory(&'static str),
    #[error("{path}: {source}")]
    Io {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
    #[error("{path}: {message}")]
    InvalidData { path: PathBuf, message: String },
    #[error("invalid skill target {0:?}")]
    InvalidTarget(String),
    #[error("skill {0:?} was not discovered")]
    NotFound(String),
    #[error("skill {0:?} is not enabled")]
    NotEnabled(String),
    #[error("skill {0:?} has an unsupported selection expression")]
    UnsupportedCondition(String),
    #[error("evaluate selection for skill {skill:?}: {source}")]
    Condition {
        skill: String,
        #[source]
        source: io::Error,
    },
    #[error("invalid line range; use positive, inclusive, 1-based bounds")]
    InvalidLineRange,
    #[error("line {0} is beyond end of file")]
    LineOutOfBounds(usize),
    #[error("remote skill patch no longer applies: {0}")]
    StaleRemotePatch(PathBuf),
}

pub type Result<T> = std::result::Result<T, Error>;

/// A read-only view of a project's skills, including inherited user selections.
pub struct SkillsManager {
    strict: bool,
    project: PathBuf,
    home: PathBuf,
    cache: PathBuf,
    codex_home: PathBuf,
    admin_skills: Option<PathBuf>,
    #[cfg(feature = "cli-support")]
    condition_fallback: Option<ConditionFallback>,
}

#[cfg(feature = "cli-support")]
type ConditionFallback = fn(&SkillsManager, &str) -> io::Result<bool>;

impl SkillsManager {
    /// Use the current user's home, cache and Codex paths for the given project.
    /// Does not infer a list harness from the process environment.
    pub fn for_project(project: impl Into<PathBuf>) -> Result<Self> {
        let home = dirs::home_dir().ok_or(Error::MissingDirectory("home"))?;
        let cache = dirs::cache_dir().ok_or(Error::MissingDirectory("cache"))?;
        let mut manager = Self::new(project, home, cache);
        if let Some(path) = env::var_os("CODEX_HOME").filter(|path| !path.is_empty()) {
            manager.codex_home = path.into();
        }
        manager.admin_skills = Some("/etc/codex/skills".into());
        Ok(manager)
    }

    /// Use explicit project, user home and user cache directories.
    /// Codex paths are relative to this home; system-wide skills are not read.
    pub fn new(
        project: impl Into<PathBuf>,
        home: impl Into<PathBuf>,
        cache: impl Into<PathBuf>,
    ) -> Self {
        let home = home.into();
        Self {
            strict: false,
            project: project.into(),
            codex_home: home.join(".codex"),
            home,
            cache: cache.into(),
            admin_skills: None,
            #[cfg(feature = "cli-support")]
            condition_fallback: None,
        }
    }

    /// Propagate permission denials instead of skipping inaccessible discovery
    /// candidates. Use in a caller-enforced sandbox to keep denied reads
    /// distinct from missing skills. This does not sandbox the process.
    pub fn with_strict_discovery(mut self) -> Self {
        self.strict = true;
        self
    }

    /// Whether a project, global, or legacy global selection file is present.
    /// An empty selection still signals opt-in. This does not validate its
    /// contents or check that any particular skill is enabled.
    pub fn is_enabled(&self) -> Result<bool> {
        selection::is_enabled(self)
    }

    /// List the skills available for model invocation, sorted by name.
    pub fn list(&self) -> Result<Vec<ListedSkill>> {
        catalog::list(self, None)
    }

    /// List for an agent, excluding other agents' roots and suppressing skills
    /// the chosen agent already loads. Does not change what get can serve.
    pub fn list_for(&self, harness: Harness) -> Result<Vec<ListedSkill>> {
        catalog::list(self, Some(harness))
    }

    /// List skills in the CLI's XML format, including grouped reference paths.
    /// Pass an empty slice to infer the active harness as the CLI does, or pass
    /// harnesses to filter explicitly.
    ///
    /// Selection expressions run in-process, without a shell wrapper. A reached
    /// external command runs with the caller's permissions, starting in the
    /// project directory unless the expression changes it with `cd`.
    /// Use this method only with trusted selections.
    pub fn list_xml(&self, harnesses: &[Harness]) -> Result<String> {
        let inferred;
        let harnesses = if harnesses.is_empty() {
            inferred = infer_harness();
            inferred.as_slice()
        } else {
            harnesses
        };
        catalog::list_xml(self, harnesses)
    }

    /// Read an enabled skill's manifest or a skill-relative file.
    /// A bare name reads SKILL.md; use name/path to read another file.
    pub fn get(&self, target: &str) -> Result<Vec<u8>> {
        self.get_impl(target, None)
    }

    /// Like get, but read a 1-based inclusive range from the served body.
    /// The end is clipped at EOF; a start beyond EOF is an error.
    pub fn get_lines(&self, target: &str, lines: RangeInclusive<usize>) -> Result<Vec<u8>> {
        if *lines.start() == 0 || lines.is_empty() {
            return Err(Error::InvalidLineRange);
        }
        self.get_impl(target, Some(lines))
    }

    fn get_impl(&self, target: &str, lines: Option<RangeInclusive<usize>>) -> Result<Vec<u8>> {
        let (name, relative) = content::split_target(target)?;
        let skill = catalog::find(self, name)?;
        content::get(self, &skill, relative, lines)
    }

    fn manager_home(&self) -> PathBuf {
        self.home.join(".skills-mgr")
    }
}

fn infer_harness() -> Option<Harness> {
    let set = |name| env::var_os(name).is_some_and(|value| !value.is_empty());
    let mut harnesses = [
        set("CLAUDECODE").then_some(Harness::Claude),
        (set("GROK_AGENT") || set("GROK_SESSION_ID")).then_some(Harness::Grok),
        set("CODEX_THREAD_ID").then_some(Harness::Codex),
    ]
    .into_iter()
    .flatten();
    let first = harnesses.next();
    if harnesses.next().is_none() {
        first
    } else {
        None
    }
}

pub fn io_error(path: &Path, source: io::Error) -> Error {
    Error::Io {
        path: path.to_owned(),
        source,
    }
}

pub fn invalid_data(path: &Path, message: impl ToString) -> Error {
    Error::InvalidData {
        path: path.to_owned(),
        message: message.to_string(),
    }
}
