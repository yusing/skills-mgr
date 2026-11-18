use regex::Regex;
use serde::Deserialize;
use std::{
    collections::HashMap,
    fs, io,
    path::{Path, PathBuf},
    sync::LazyLock,
};

#[derive(Default)]
pub(super) struct Dependencies(HashMap<String, Vec<String>>);

#[derive(Default, Deserialize)]
#[serde(default, rename_all = "camelCase")]
struct Package {
    dependencies: Option<HashMap<String, String>>,
    dev_dependencies: Option<HashMap<String, String>>,
    optional_dependencies: Option<HashMap<String, String>>,
}

#[derive(Default, Deserialize)]
#[serde(default, rename_all = "kebab-case")]
struct Tables {
    dependencies: toml::Table,
    dev_dependencies: toml::Table,
    build_dependencies: toml::Table,
}

#[derive(Default, Deserialize)]
#[serde(default)]
struct Workspace {
    dependencies: toml::Table,
}

#[derive(Deserialize)]
struct Cargo {
    #[serde(flatten)]
    root: Tables,
    #[serde(default)]
    target: HashMap<String, Tables>,
    #[serde(default)]
    workspace: Workspace,
}

impl Dependencies {
    pub(super) fn load(paths: &[PathBuf]) -> io::Result<Self> {
        let mut deps = Self::default();
        for path in paths {
            deps.add_manifest(path)
                .map_err(|e| io::Error::new(e.kind(), format!("{}: {e}", path.display())))?;
        }
        Ok(deps)
    }

    fn add_manifest(&mut self, path: &Path) -> io::Result<()> {
        let src = fs::read_to_string(path)?;
        let mut add = |name: String, version: String| self.0.entry(name).or_default().push(version);
        match path.file_name().and_then(|n| n.to_str()) {
            Some("package.json") if !src.trim().is_empty() => {
                let pkg: Package = serde_json::from_str(&src)?;
                for (name, version) in [
                    pkg.dependencies,
                    pkg.dev_dependencies,
                    pkg.optional_dependencies,
                ]
                .into_iter()
                .flatten()
                .flatten()
                {
                    add(name, version);
                }
            }
            Some("Cargo.toml") => {
                let cargo: Cargo = toml::from_str(&src).map_err(io::Error::other)?;
                for table in std::iter::once(cargo.workspace.dependencies).chain(
                    std::iter::once(cargo.root)
                        .chain(cargo.target.into_values())
                        .flat_map(|t| [t.dependencies, t.dev_dependencies, t.build_dependencies]),
                ) {
                    for (name, value) in table {
                        match value {
                            toml::Value::String(v) => add(name, v),
                            toml::Value::Table(t) => add(
                                name,
                                t.get("version")
                                    .and_then(|v| v.as_str())
                                    .unwrap_or_default()
                                    .into(),
                            ),
                            _ => {}
                        }
                    }
                }
            }
            Some("go.mod") => {
                let manifest = src
                    .parse::<gomod_parser::GoMod>()
                    .map_err(io::Error::other)?;
                for dep in manifest.require.into_iter().filter(|d| !d.indirect) {
                    add(dep.module.module_path, dep.module.version);
                }
            }
            _ => {}
        }
        Ok(())
    }

    pub(super) fn matches(&self, name: &str, constraint: Option<&str>) -> io::Result<bool> {
        let Some(expr) = constraint else {
            return Ok(self.0.contains_key(name));
        };
        let groups = expr
            .split("||")
            .map(|group| {
                group
                    .split("&&")
                    .map(Constraint::parse)
                    .collect::<io::Result<Vec<_>>>()
            })
            .collect::<io::Result<Vec<_>>>()?;
        Ok(self.0.get(name).is_some_and(|requirements| {
            requirements.iter().any(|req| {
                groups.iter().any(|group| {
                    req.trim()
                        .strip_prefix("workspace:")
                        .unwrap_or(req.trim())
                        .split("||")
                        .all(|alt| {
                            let mut bounds = BOUNDS.captures_iter(alt).peekable();
                            bounds.peek().is_some()
                                && bounds.all(|bound| {
                                    let (version, _) = version(&bound);
                                    group.iter().all(|test| test.matches(&version))
                                })
                        })
                })
            })
        }))
    }
}

static WANT: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^(>=|==|<=|<)[[:space:]]*v?([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?$").unwrap()
});
static BOUNDS: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(
        r"(?:^|[[:space:],])([<>]=?|[~^=])?[[:space:]]*v?([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?",
    )
    .unwrap()
});

fn version(parts: &regex::Captures<'_>) -> ([u64; 3], usize) {
    let mut nums = [0; 3];
    let mut precision = 0;
    for (i, n) in nums.iter_mut().enumerate() {
        if let Some(part) = parts.get(i + 2) {
            *n = part.as_str().parse().unwrap_or(u64::MAX);
            precision = i + 1;
        }
    }
    (nums, precision)
}

struct Constraint<'a> {
    op: &'a str,
    version: [u64; 3],
    precision: usize,
}
impl<'a> Constraint<'a> {
    fn parse(src: &'a str) -> io::Result<Self> {
        let parts = WANT.captures(src.trim()).ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("invalid has_dependency constraint: {src:?}"),
            )
        })?;
        let (version, precision) = version(&parts);
        Ok(Self {
            op: parts.get(1).unwrap().as_str(),
            version,
            precision,
        })
    }
    fn matches(&self, actual: &[u64; 3]) -> bool {
        let cmp = actual[..self.precision].cmp(&self.version[..self.precision]);
        match self.op {
            ">=" => cmp.is_ge(),
            "<=" => cmp.is_le(),
            "==" => cmp.is_eq(),
            _ => cmp.is_lt(),
        }
    }
}
