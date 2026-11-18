use crate::SkillsManager;
use ignore::gitignore::{Gitignore, GitignoreBuilder};
use std::{
    collections::HashSet,
    fs, io,
    path::{Path, PathBuf},
};

#[derive(Clone, Copy, Hash, Eq, PartialEq)]
pub(super) enum Feature {
    Language(&'static str),
    Tool(&'static str),
}

// Language, exact filenames, case-insensitive extensions. Uppercase .C is C++.
const LANGUAGES: &[(&str, &[&str], &[&str])] = &[
    ("go", &["go.mod"], &["go"]),
    ("rust", &["Cargo.toml"], &["rs"]),
    ("node", &["package.json"], &[]),
    (
        "typescript",
        &["tsconfig.json"],
        &["ts", "tsx", "mts", "cts"],
    ),
    ("tsx", &[], &["tsx"]),
    ("javascript", &[], &["js", "jsx", "mjs", "cjs"]),
    ("jsx", &[], &["jsx"]),
    ("html", &[], &["html", "htm"]),
    ("css", &[], &["css"]),
    (
        "python",
        &["pyproject.toml", "requirements.txt", "Pipfile"],
        &["py", "pyw"],
    ),
    ("c", &[], &["c"]),
    ("c++", &[], &["cc", "cpp", "cxx", "c++", "hh", "hpp", "hxx"]),
    ("c#", &[], &["cs", "csproj"]),
    ("java", &[], &["java"]),
    ("lua", &[], &["lua"]),
    ("vb", &[], &["vb", "vbproj"]),
    ("php", &["composer.json"], &["php"]),
    ("r", &[], &["r", "rmd", "rproj"]),
    ("ruby", &["Gemfile"], &["rb"]),
    ("swift", &["Package.swift"], &["swift"]),
    ("perl", &["cpanfile"], &["pl", "pm"]),
    ("assembly", &[], &["asm", "s"]),
    ("shell", &[], &["sh"]),
    ("bash", &[], &["bash"]),
    (
        "postgres",
        &["postgresql.conf", "pg_hba.conf", "pg_ident.conf"],
        &["psql"],
    ),
    ("sql", &[], &["sql"]),
    ("yaml", &[], &["yaml", "yml"]),
    ("json", &[], &["json"]),
    ("toml", &[], &["toml"]),
    ("ini", &[], &["ini"]),
];

const COMPOSE: &[&str] = &[
    "compose.yml",
    "compose.yaml",
    "docker-compose.yml",
    "docker-compose.yaml",
];
const TOOLS: &[(&str, &[&str])] = &[
    ("bun", &["bun.lock", "bun.lockb", "bunfig.toml"]),
    (
        "yarn",
        &["yarn.lock", ".yarnrc", ".yarnrc.yml", ".yarnrc.yaml"],
    ),
    ("deno", &["deno.json", "deno.jsonc", "deno.lock"]),
    ("npm", &["package-lock.json", "npm-shrinkwrap.json"]),
    ("pnpm", &["pnpm-lock.yaml", "pnpm-workspace.yaml"]),
    ("maven", &["pom.xml", "mvnw", "mvnw.cmd"]),
    ("composer", &["composer.json", "composer.lock"]),
    (
        "cmake",
        &[
            "CMakeLists.txt",
            "CMakePresets.json",
            "CMakeUserPresets.json",
        ],
    ),
    ("make", &["Makefile", "makefile", "GNUmakefile"]),
    ("just", &["justfile", "Justfile", ".justfile"]),
    ("shadowtree", &[".shadowtree.toml"]),
    (
        "taskfile",
        &[
            "Taskfile.yml",
            "Taskfile.yaml",
            "taskfile.yml",
            "taskfile.yaml",
        ],
    ),
    (
        "bazel",
        &[
            ".bazelrc",
            "MODULE.bazel",
            "WORKSPACE",
            "WORKSPACE.bazel",
            "BUILD",
            "BUILD.bazel",
        ],
    ),
    (
        "docker",
        &["Dockerfile", "docker-bake.hcl", "docker-bake.json"],
    ),
    (
        "kubernetes",
        &[
            "kustomization.yml",
            "kustomization.yaml",
            "Kustomization",
            "Chart.yaml",
            "skaffold.yml",
            "skaffold.yaml",
        ],
    ),
    ("pip", &["pip.conf", "pip.ini"]),
    ("uv", &["uv.lock", "uv.toml"]),
    ("docker-compose", COMPOSE),
];

impl Feature {
    pub(super) fn parse(cmd: &str, name: &str) -> Option<Self> {
        if cmd == "lang" {
            let name = match name {
                "js" => "javascript",
                "ts" => "typescript",
                "asm" => "assembly",
                "sh" => "shell",
                n => n,
            };
            LANGUAGES
                .iter()
                .find(|l| l.0 == name)
                .map(|l| Self::Language(l.0))
        } else {
            let name = if name == "k8s" { "kubernetes" } else { name };
            TOOLS.iter().find(|t| t.0 == name).map(|t| Self::Tool(t.0))
        }
    }
}

#[derive(Default)]
pub(super) struct Evidence {
    pub(super) features: HashSet<Feature>,
    pub(super) manifests: Vec<PathBuf>,
}

impl Evidence {
    fn record(&mut self, path: PathBuf) {
        let Some(file) = path.file_name() else { return };
        let name = file.to_string_lossy();
        let name = name.as_ref();
        let ext = name.rsplit_once('.').map_or("", |(_, ext)| ext);
        for &(lang, markers, extensions) in LANGUAGES {
            if markers.contains(&name)
                || if ext == "C" {
                    lang == "c++"
                } else {
                    extensions.iter().any(|e| ext.eq_ignore_ascii_case(e))
                }
            {
                self.features.insert(Feature::Language(lang));
            }
        }
        for &(tool, markers) in TOOLS {
            if markers.contains(&name)
                || match tool {
                    "docker" => name.starts_with("Dockerfile.") || COMPOSE.contains(&name),
                    "pip" => name.starts_with("requirements") && name.ends_with(".txt"),
                    _ => false,
                }
            {
                self.features.insert(Feature::Tool(tool));
            }
        }
        if matches!(name, "go.mod" | "Cargo.toml" | "package.json") {
            self.manifests.push(path);
        }
    }

    pub(super) fn scan(manager: &SkillsManager) -> io::Result<Self> {
        let project = manager.project.canonicalize()?;
        let recursive = manager.home.canonicalize().ok().as_ref() != Some(&project);
        let mut evidence = Self::default();
        let mut tracked = HashSet::new();
        let mut rules = Vec::new();
        let mut ignored = false;
        if let Some((root, git, index)) = repository(&project) {
            for entry in index.entries() {
                let rel = gix_path::from_bstr(entry.path(&index)).map_err(io::Error::other)?;
                let path = root.join(rel);
                if path.starts_with(&project) {
                    for path in path.ancestors().take_while(|p| p.starts_with(&project)) {
                        if !tracked.insert(path.to_owned()) {
                            break;
                        }
                    }
                }
            }
            load_rules(&mut rules, &root, &git.join("info/exclude"))?;
            let ancestors: Vec<_> = project.ancestors().take_while(|p| *p != root).collect();
            for dir in ancestors.into_iter().rev() {
                let parent = dir.parent().unwrap();
                load_rules(&mut rules, parent, &parent.join(".gitignore"))?;
                ignored |= is_ignored(&rules, dir, true);
            }
        }
        if !ignored || tracked.contains(&project) {
            evidence.walk(&project, recursive, &mut rules, &tracked, ignored)?;
        }
        evidence.manifests.sort();
        Ok(evidence)
    }

    fn walk(
        &mut self,
        dir: &Path,
        recursive: bool,
        rules: &mut Vec<Gitignore>,
        tracked: &HashSet<PathBuf>,
        parent_ignored: bool,
    ) -> io::Result<()> {
        let entries = match fs::read_dir(dir) {
            Err(e) if e.kind() == io::ErrorKind::PermissionDenied => return Ok(()),
            result => result?,
        };
        let depth = rules.len();
        if !parent_ignored {
            load_rules(rules, dir, &dir.join(".gitignore"))?;
        }
        for entry in entries {
            let entry = entry?;
            let path = entry.path();
            let is_dir = entry.file_type()?.is_dir();
            if is_dir
                && (!recursive
                    || matches!(
                        entry.file_name().to_str(),
                        Some(".git" | "node_modules" | "target")
                    ))
            {
                continue;
            }
            let ignored = parent_ignored || is_ignored(rules, &path, is_dir);
            if ignored && !tracked.contains(&path) {
                continue;
            }
            if is_dir {
                self.walk(&path, true, rules, tracked, ignored)?;
            } else {
                self.record(path);
            }
        }
        rules.truncate(depth);
        Ok(())
    }
}

fn repository(project: &Path) -> Option<(PathBuf, PathBuf, gix_index::File)> {
    let (git, root) = gix_discover::upwards(project)
        .ok()?
        .0
        .into_repository_and_work_tree_directories();
    let root = root.filter(|root| project.starts_with(root))?;
    let common = gix_discover::path::from_plain_file_relative_to_file(&git.join("commondir"))
        .transpose()
        .ok()?
        .unwrap_or_else(|| git.clone());
    let index = [gix_hash::Kind::Sha1, gix_hash::Kind::Sha256]
        .into_iter()
        .find_map(|kind| {
            gix_index::File::at_or_default(git.join("index"), kind, false, Default::default()).ok()
        })?;
    Some((root, common, index))
}

fn load_rules(rules: &mut Vec<Gitignore>, dir: &Path, path: &Path) -> io::Result<()> {
    let data = match fs::read_to_string(path) {
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        result => result?,
    };
    let mut builder = GitignoreBuilder::new(dir);
    for line in data.lines() {
        let _ = builder.add_line(None, line);
    }
    rules.push(builder.build().map_err(io::Error::other)?);
    Ok(())
}

fn is_ignored(rules: &[Gitignore], path: &Path, dir: bool) -> bool {
    rules
        .iter()
        .rev()
        .map(|rule| rule.matched(path, dir))
        .find(|m| !m.is_none())
        .is_some_and(|m| m.is_ignore())
}
