use std::{
    collections::{BTreeMap, HashSet},
    env,
    fs::{self, File},
    io::{self, Read},
    path::{Path, PathBuf},
    time::Duration,
};

use jiff::Timestamp;
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use url::Url;

use crate::{SkillsManager, io_error, remote::Provider};

use super::{Result, cancel, mutation};

const LIMIT: usize = 4 * 1024 * 1024;
pub(super) const TTL: i64 = 5 * 60;
const TOPICS: &[(&str, &str, &str)] = &[
    ("react", "Frontend & React", "react"),
    ("nextjs", "Next.js", "next.js"),
    ("design", "Design & UI", "design ui"),
    ("mobile", "Mobile", "mobile"),
    ("agent-workflows", "Agent workflows", "agent workflows"),
    ("databases", "Databases", "databases"),
    ("testing", "Testing", "testing"),
    ("marketing", "Marketing", "marketing"),
];

#[derive(Default, Deserialize, Serialize)]
#[serde(default)]
struct SkillsShSkill {
    id: String,
    name: String,
    description: String,
    source: String,
    installs: i64,
}

#[derive(Default, Deserialize, Serialize)]
#[serde(default, rename_all = "camelCase")]
struct SkillsMpSkill {
    id: String,
    name: String,
    author: String,
    description: String,
    #[serde(rename = "githubUrl")]
    github_url: String,
    stars: i64,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct RegistryCache {
    schema_revision: u32,
    updated_at: String,
    topics: Vec<Topic>,
}

#[derive(Deserialize, Serialize)]
struct Topic {
    slug: String,
    name: String,
    skills: Vec<SkillsShSkill>,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct SkillsMpCache {
    schema_revision: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    updated_at: Option<String>,
    #[serde(default)]
    skills: Vec<SkillsMpSkill>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    searches: BTreeMap<String, Search>,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct Search {
    updated_at: String,
    skills: Vec<SkillsMpSkill>,
}

#[derive(Serialize)]
#[serde(rename_all = "PascalCase")]
pub(super) struct SearchRow {
    #[serde(rename = "ID")]
    id: String,
    name: String,
    description: String,
    label: String,
    provider: Provider,
    locator: String,
}

impl From<SkillsShSkill> for SearchRow {
    fn from(skill: SkillsShSkill) -> Self {
        Self {
            locator: skill.id.clone(),
            id: skill.id,
            name: skill.name,
            description: skill.description,
            label: format!("{} • {} installs", skill.source, skill.installs),
            provider: Provider::SkillsSh,
        }
    }
}

impl From<SkillsMpSkill> for SearchRow {
    fn from(skill: SkillsMpSkill) -> Self {
        Self {
            id: skill.id,
            name: skill.name,
            description: skill.description,
            label: format!("{} • {} stars", skill.author, skill.stars),
            provider: Provider::SkillsMp,
            locator: skill.github_url,
        }
    }
}

pub(super) fn browse(
    manager: &SkillsManager,
    provider: Provider,
    query: Option<&str>,
) -> Result<Vec<SearchRow>> {
    let agent = http_agent();
    let query = query.map(|query| {
        query
            .split_whitespace()
            .collect::<Vec<_>>()
            .join(" ")
            .to_lowercase()
    });
    match (provider, query.as_deref()) {
        (Provider::SkillsSh, Some(query)) => Ok(skills_sh(&agent, query)?
            .into_iter()
            .map(Into::into)
            .collect()),
        (Provider::SkillsMp, query) => Ok(skills_mp(manager, &agent, query)?
            .into_iter()
            .map(Into::into)
            .collect()),
        _ => Err("registry operation needs a supported provider and query".into()),
    }
}

pub(super) fn refresh_topics(manager: &SkillsManager, now: Timestamp) -> Result<()> {
    let path = cache_file(manager, "skills-sh.json");
    if read_cache::<RegistryCache>(&path).is_ok_and(|cache| {
        cache.is_some_and(|cache| cache.schema_revision == 1 && fresh(&cache.updated_at, now))
    }) {
        return Ok(());
    }
    let agent = http_agent();
    let mut topics = Vec::with_capacity(TOPICS.len());
    for &(slug, name, query) in TOPICS {
        cancel::check()?;
        let skills =
            skills_sh(&agent, query).map_err(|err| format!("refresh {name} topic: {err}"))?;
        topics.push(Topic {
            slug: slug.into(),
            name: name.into(),
            skills,
        });
    }
    cancel::check()?;
    write_cache(
        &path,
        &RegistryCache {
            schema_revision: 1,
            updated_at: now.to_string(),
            topics,
        },
    )
}

#[derive(Deserialize)]
#[serde(bound(deserialize = "T: Deserialize<'de>"))]
struct SkillList<T> {
    #[serde(default)]
    skills: Vec<T>,
}

fn skills_sh(agent: &ureq::Agent, query: &str) -> Result<Vec<SkillsShSkill>> {
    let result: SkillList<SkillsShSkill> = fetch(
        agent,
        "https://skills.sh/api/search",
        &[("q", query), ("limit", "200")],
        None,
    )?;
    let mut seen = HashSet::new();
    let mut skills = result.skills;
    skills.retain(|skill| {
        !skill.id.is_empty() && !skill.name.is_empty() && seen.insert(skill.id.clone())
    });
    skills
        .sort_by_cached_key(|skill| (std::cmp::Reverse(skill.installs), skill.name.to_lowercase()));
    Ok(skills)
}

fn skills_mp(
    manager: &SkillsManager,
    agent: &ureq::Agent,
    query: Option<&str>,
) -> Result<Vec<SkillsMpSkill>> {
    let path = cache_file(
        manager,
        if query.is_some() {
            "skillsmp-search.json"
        } else {
            "skillsmp.json"
        },
    );
    if let Some(query) = query
        && let Some(mut cache) = read_skills_mp(&path)
        && let Some(hit) = cache.searches.remove(query)
        && fresh(&hit.updated_at, Timestamp::now())
    {
        return Ok(unique_skills_mp(hit.skills));
    }
    let key = env::var("SKILLSMP_API_KEY")
        .ok()
        .filter(|key| !key.is_empty());
    let (endpoint, params) = if let Some(query) = query {
        (
            "https://skillsmp.com/api/v1/skills/search",
            vec![
                ("q", query),
                ("page", "1"),
                ("limit", "50"),
                ("sortBy", "stars"),
            ],
        )
    } else {
        (
            "https://skillsmp.com/api/skills",
            vec![("page", "1"), ("limit", "48"), ("sortBy", "stars")],
        )
    };
    #[derive(Deserialize)]
    struct Response {
        #[serde(default)]
        skills: Vec<SkillsMpSkill>,
        data: Option<SkillList<SkillsMpSkill>>,
    }
    let response: Response = fetch(agent, endpoint, &params, key.as_deref())?;
    let skills = unique_skills_mp(if query.is_some() {
        response.data.map(|data| data.skills).unwrap_or_default()
    } else {
        response.skills
    });
    // The child can terminate immediately while waiting on the network. Only
    // the short cache transaction needs graceful cancellation.
    cancel::install()?;
    let now = Timestamp::now();
    if let Some(query) = query {
        let _guard = mutation::exclusive_lock(&path.with_extension("lock"))?;
        let mut cache = read_skills_mp(&path).unwrap_or_else(|| SkillsMpCache {
            schema_revision: 1,
            updated_at: None,
            skills: Vec::new(),
            searches: BTreeMap::new(),
        });
        cache.searches.retain(|_, hit| fresh(&hit.updated_at, now));
        cache.searches.insert(
            query.to_owned(),
            Search {
                updated_at: now.to_string(),
                skills,
            },
        );
        while cache.searches.len() > 16 {
            let oldest = cache
                .searches
                .iter()
                .min_by_key(|(_, hit)| hit.updated_at.parse::<Timestamp>().ok())
                .map(|(query, _)| query.clone())
                .expect("cache is nonempty");
            cache.searches.remove(&oldest);
        }
        cancel::check()?;
        write_cache(&path, &cache)?;
        Ok(cache
            .searches
            .remove(query)
            .map(|hit| hit.skills)
            .unwrap_or_default())
    } else {
        let cache = SkillsMpCache {
            schema_revision: 1,
            updated_at: Some(now.to_string()),
            skills,
            searches: BTreeMap::new(),
        };
        cancel::check()?;
        write_cache(&path, &cache)?;
        Ok(cache.skills)
    }
}

fn read_skills_mp(path: &Path) -> Option<SkillsMpCache> {
    read_cache::<SkillsMpCache>(path)
        .ok()
        .flatten()
        .filter(|cache| cache.schema_revision == 1)
}

fn unique_skills_mp(mut skills: Vec<SkillsMpSkill>) -> Vec<SkillsMpSkill> {
    let mut ids = HashSet::new();
    let mut names = HashSet::new();
    skills.retain(|skill| {
        if skill.id.is_empty() || skill.name.is_empty() {
            return false;
        }
        let id = ids.insert(skill.id.clone());
        let name = names.insert((skill.author.clone(), skill.name.clone()));
        id && name
    });
    skills
}

fn http_agent() -> ureq::Agent {
    ureq::Agent::config_builder()
        .https_only(true)
        .timeout_global(Some(Duration::from_secs(30)))
        .http_status_as_error(false)
        .build()
        .into()
}

fn fetch<T: DeserializeOwned>(
    agent: &ureq::Agent,
    endpoint: &str,
    parameters: &[(&str, &str)],
    key: Option<&str>,
) -> Result<T> {
    let mut url = Url::parse(endpoint)?;
    url.query_pairs_mut()
        .extend_pairs(parameters.iter().copied());
    let mut request = agent.get(url.as_str()).header("Accept", "application/json");
    if let Some(key) = key {
        if key.bytes().any(|byte| !byte.is_ascii_graphic()) {
            return Err("invalid SkillsMP API key format".into());
        }
        request = request.header("Authorization", format!("Bearer {key}"));
    }
    let response = request.call()?;
    let status = response.status();
    let mut data = Vec::new();
    response
        .into_body()
        .into_reader()
        .take(LIMIT as u64 + 1)
        .read_to_end(&mut data)?;
    if data.len() > LIMIT {
        return Err(format!("registry response exceeds {LIMIT} bytes").into());
    }
    if status.as_u16() != 200 {
        // A provider error body can reflect credentials. Report the status,
        // not untrusted body text.
        return Err(format!("unexpected registry HTTP status {status}").into());
    }
    Ok(serde_json::from_slice(&data).map_err(|err| format!("decode registry response: {err}"))?)
}

pub(super) fn fresh(value: &str, now: Timestamp) -> bool {
    value
        .parse::<Timestamp>()
        .is_ok_and(|at| at <= now && at.duration_until(now).as_secs() < TTL)
}

fn cache_file(manager: &SkillsManager, name: &str) -> PathBuf {
    manager.cache.join("skills-mgr").join(name)
}

fn read_cache<T: DeserializeOwned>(path: &Path) -> Result<Option<T>> {
    let file = match File::open(path) {
        Ok(file) => file,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(io_error(path, err).into()),
    };
    let mut data = Vec::new();
    file.take(LIMIT as u64 + 1)
        .read_to_end(&mut data)
        .map_err(|err| io_error(path, err))?;
    if data.len() > LIMIT {
        return Err(format!("registry cache {} exceeds {LIMIT} bytes", path.display()).into());
    }
    Ok(Some(serde_json::from_slice(&data).map_err(|err| {
        format!("decode {}: {err}", path.display())
    })?))
}

fn write_cache(path: &Path, value: &impl Serialize) -> Result<()> {
    let mut bytes = serde_json::to_vec(value)?;
    bytes.push(b'\n');
    if bytes.len() > LIMIT {
        return Err(format!("registry cache {} exceeds {LIMIT} bytes", path.display()).into());
    }
    let parent = path.parent().expect("registry cache has a parent");
    fs::create_dir_all(parent).map_err(|err| io_error(parent, err))?;
    mutation::atomic_write(path, &bytes, 0o600)
}
