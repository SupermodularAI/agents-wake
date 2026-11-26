//! Installed skills and agents: local, plugin and APM-deployed. Stat and read only.

use crate::event::{is_token, Kind};
use crate::harness::{PrimitiveInfo, PrimitiveLookup};
use serde_json::Value;
use std::collections::HashSet;
use std::fs;
use std::path::Path;

pub struct Installed {
    pub items: Vec<PrimitiveInfo>,
}

/// One package from `apm.lock.yaml`: its name, version and what it deployed.
struct ApmPackage {
    name: String,
    version: Option<String>,
    skills: Vec<String>,
}

impl Installed {
    pub fn scan(home: &Path, project_dir: &Path) -> Installed {
        let apm: Vec<ApmPackage> = [
            project_dir.join("apm.lock.yaml"),
            home.join(".apm/apm.lock.yaml"),
        ]
        .iter()
        .flat_map(|p| parse_apm_lock(p))
        .collect();
        let mut items = Vec::new();
        for base in [project_dir.join(".claude"), home.join(".claude")] {
            scan_dir(&base, None, &apm, &mut items);
        }
        scan_plugins(
            &home.join(".claude/plugins/installed_plugins.json"),
            &mut items,
        );

        let mut seen = HashSet::new();
        items.retain(|i| is_token(&i.name) && seen.insert((i.kind == Kind::Skill, i.name.clone())));
        Installed { items }
    }

    /// `skill:<name>@<version or ->` and `agent:...`, sorted.
    pub fn inventory_strings(&self) -> Vec<String> {
        let mut out: Vec<String> = self
            .items
            .iter()
            .map(|i| {
                let prefix = if i.kind == Kind::Skill {
                    "skill"
                } else {
                    "agent"
                };
                format!(
                    "{prefix}:{}@{}",
                    i.name,
                    i.version.as_deref().unwrap_or("-")
                )
            })
            .collect();
        out.sort();
        out
    }

    fn find(&self, kind: Kind, name: &str) -> Option<PrimitiveInfo> {
        if let Some(i) = self.items.iter().find(|i| i.kind == kind && i.name == name) {
            return Some(i.clone());
        }
        if name.contains(':') {
            return None;
        }
        let suffix = format!(":{name}");
        let mut m = self
            .items
            .iter()
            .filter(|i| i.kind == kind && i.name.ends_with(&suffix));
        match (m.next(), m.next()) {
            (Some(one), None) => Some(one.clone()),
            _ => None,
        }
    }
}

impl PrimitiveLookup for Installed {
    fn skill(&self, name: &str) -> Option<PrimitiveInfo> {
        self.find(Kind::Skill, name)
    }
    fn agent(&self, name: &str) -> Option<PrimitiveInfo> {
        self.find(Kind::Subagent, name)
    }
}

fn tokens_for(path: &Path) -> Option<u64> {
    fs::metadata(path).ok().map(|m| m.len().div_ceil(4))
}

fn sorted_entries(dir: &Path) -> Vec<fs::DirEntry> {
    let mut v: Vec<_> = fs::read_dir(dir).into_iter().flatten().flatten().collect();
    v.sort_by_key(|e| e.file_name());
    v
}

/// Reads `<base>/skills/*/SKILL.md` and `<base>/agents/*.md`. `plugin` is
/// `(plugin, marketplace, version)` for plugin installs, `None` for local ones.
fn scan_dir(
    base: &Path,
    plugin: Option<(&str, &str, Option<&str>)>,
    apm: &[ApmPackage],
    out: &mut Vec<PrimitiveInfo>,
) {
    let qualify = |n: &str| match plugin {
        Some((p, _, _)) => format!("{p}:{n}"),
        None => n.to_string(),
    };
    for e in sorted_entries(&base.join("skills")) {
        let file = e.path().join("SKILL.md");
        let Some(dir) = e.file_name().to_str().map(str::to_string) else {
            continue;
        };
        if !file.is_file() {
            continue;
        }
        let mut info = info(Kind::Skill, qualify(&dir), plugin, tokens_for(&file));
        if plugin.is_none() {
            if let Some(pkg) = apm.iter().find(|p| p.skills.contains(&dir)) {
                info.version = pkg.version.clone();
                info.plugin = Some(pkg.name.clone());
            }
        }
        out.push(info);
    }
    for e in sorted_entries(&base.join("agents")) {
        let path = e.path();
        if path.extension().and_then(|x| x.to_str()) != Some("md") || !path.is_file() {
            continue;
        }
        let Some(stem) = path.file_stem().and_then(|s| s.to_str()) else {
            continue;
        };
        out.push(info(
            Kind::Subagent,
            qualify(stem),
            plugin,
            tokens_for(&path),
        ));
    }
}

fn info(
    kind: Kind,
    name: String,
    plugin: Option<(&str, &str, Option<&str>)>,
    context_tokens: Option<u64>,
) -> PrimitiveInfo {
    PrimitiveInfo {
        kind,
        name,
        version: plugin.and_then(|p| p.2).map(str::to_string),
        plugin: plugin.map(|p| p.0.to_string()),
        marketplace: plugin.map(|p| p.1.to_string()),
        context_tokens,
    }
}

fn scan_plugins(file: &Path, out: &mut Vec<PrimitiveInfo>) {
    let Some(json) = fs::read_to_string(file)
        .ok()
        .and_then(|s| serde_json::from_str::<Value>(&s).ok())
    else {
        return;
    };
    let Some(plugins) = json.get("plugins").and_then(Value::as_object) else {
        return;
    };
    let mut keys: Vec<_> = plugins.keys().collect();
    keys.sort();
    for key in keys {
        let Some((plugin, marketplace)) = key.split_once('@') else {
            continue;
        };
        for entry in plugins[key].as_array().into_iter().flatten() {
            let Some(path) = entry.get("installPath").and_then(Value::as_str) else {
                continue;
            };
            let version = entry.get("version").and_then(Value::as_str);
            scan_dir(
                Path::new(path),
                Some((plugin, marketplace, version)),
                &[],
                out,
            );
        }
    }
}

/// Line-based reader for the structure APM writes: a top-level `dependencies:`
/// list whose items carry `name:`, `version:` and a `deployed_files:` list.
/// Missing or unreadable files give no packages.
fn parse_apm_lock(path: &Path) -> Vec<ApmPackage> {
    let Ok(text) = fs::read_to_string(path) else {
        return Vec::new();
    };
    let unquote = |v: &str| v.trim().trim_matches(|c| c == '\'' || c == '"').to_string();
    let mut pkgs: Vec<ApmPackage> = Vec::new();
    let mut in_deps = false;
    let mut in_files = false;
    for line in text.lines() {
        if !line.starts_with(' ') && !line.starts_with('-') {
            in_deps = line.trim_end() == "dependencies:";
            in_files = false;
            continue;
        }
        if !in_deps {
            continue;
        }
        let (item_start, body) = match line.strip_prefix("- ") {
            Some(rest) => (true, rest),
            None => (false, line.trim_start()),
        };
        if item_start {
            pkgs.push(ApmPackage {
                name: String::new(),
                version: None,
                skills: Vec::new(),
            });
            in_files = false;
        }
        let Some(pkg) = pkgs.last_mut() else { continue };
        if let Some(f) = body.strip_prefix("- ") {
            if in_files {
                // `.claude/skills/<name>` or `.claude/skills/<name>/...`
                if let Some(rest) = f.trim().strip_prefix(".claude/skills/") {
                    let name = rest.split('/').next().unwrap_or("");
                    if !name.is_empty() && !pkg.skills.iter().any(|s| s == name) {
                        pkg.skills.push(name.to_string());
                    }
                }
            }
            continue;
        }
        in_files = false;
        if let Some(v) = body.strip_prefix("name:") {
            pkg.name = unquote(v);
        } else if let Some(v) = body.strip_prefix("version:") {
            pkg.version = Some(unquote(v)).filter(|v| !v.is_empty());
        } else if body.starts_with("deployed_files:") {
            in_files = true;
        }
    }
    pkgs.retain(|p| !p.name.is_empty());
    pkgs
}
