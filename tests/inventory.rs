use std::fs;
use std::path::{Path, PathBuf};
use wake::event::Kind;
use wake::harness::claude_code::inventory::Installed;
use wake::harness::PrimitiveLookup;

fn copy_dir(from: &Path, to: &Path) {
    fs::create_dir_all(to).unwrap();
    for e in fs::read_dir(from).unwrap() {
        let e = e.unwrap();
        let dest = to.join(e.file_name());
        if e.file_type().unwrap().is_dir() {
            copy_dir(&e.path(), &dest);
        } else {
            fs::copy(e.path(), dest).unwrap();
        }
    }
}

/// Copies the synthetic home into a tempdir and writes installed_plugins.json
/// with absolute install paths.
fn home() -> (tempfile::TempDir, PathBuf) {
    let tmp = tempfile::tempdir().unwrap();
    let home = tmp.path().join("home");
    let src = Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/claude_code/home");
    copy_dir(&src, &home);
    let tpl = home.join(".claude/plugins/installed_plugins.json.template");
    let json = fs::read_to_string(&tpl)
        .unwrap()
        .replace("\"plugins/", &format!("\"{}/plugins/", home.display()));
    fs::remove_file(&tpl).unwrap();
    fs::write(home.join(".claude/plugins/installed_plugins.json"), json).unwrap();
    (tmp, home)
}

fn project(tmp: &tempfile::TempDir) -> PathBuf {
    let p = tmp.path().join("project");
    fs::create_dir_all(&p).unwrap();
    p
}

#[test]
fn finds_local_and_plugin_skills() {
    let (tmp, home) = home();
    let inst = Installed::scan(&home, &project(&tmp));
    let b = inst.skill("brainstorming").unwrap();
    assert_eq!(b.name, "superpowers:brainstorming");
    assert_eq!(b.kind, Kind::Skill);
    assert_eq!(b.version.as_deref(), Some("7.0.2-22f0b463e7a5"));
    assert_eq!(b.plugin.as_deref(), Some("superpowers"));
    assert_eq!(b.marketplace.as_deref(), Some("anthropic-plugin-directory"));
    let l = inst.skill("local-skill").unwrap();
    assert_eq!(l.plugin, None);
    assert_eq!(
        inst.agent("code-reviewer").unwrap().name,
        "superpowers:code-reviewer"
    );
    assert_eq!(inst.agent("helper").unwrap().kind, Kind::Subagent);
    assert!(inst.skill("helper").is_none());
}

#[test]
fn context_tokens_from_size() {
    let (tmp, home) = home();
    fs::write(
        home.join(".claude/skills/local-skill/SKILL.md"),
        vec![b'x'; 2840],
    )
    .unwrap();
    let inst = Installed::scan(&home, &project(&tmp));
    assert_eq!(inst.skill("local-skill").unwrap().context_tokens, Some(710));
}

#[test]
fn ambiguous_suffix_returns_none() {
    let (tmp, home) = home();
    let inst = Installed::scan(&home, &project(&tmp));
    assert!(inst.skill("review").is_none());
    assert!(inst.skill("superpowers:review").is_some());
}

#[test]
fn project_wins_over_user_and_bad_names_are_skipped() {
    let (tmp, home) = home();
    let p = project(&tmp);
    fs::create_dir_all(p.join(".claude/skills/local-skill")).unwrap();
    fs::write(p.join(".claude/skills/local-skill/SKILL.md"), vec![b'x'; 8]).unwrap();
    fs::create_dir_all(home.join(".claude/skills/bad name")).unwrap();
    fs::write(home.join(".claude/skills/bad name/SKILL.md"), "x").unwrap();
    let inst = Installed::scan(&home, &p);
    assert_eq!(inst.skill("local-skill").unwrap().context_tokens, Some(2));
    assert_eq!(
        inst.items
            .iter()
            .filter(|i| i.name == "local-skill")
            .count(),
        1
    );
    assert!(!inst.items.iter().any(|i| i.name.contains(' ')));
}

#[test]
fn scan_never_fails() {
    let tmp = tempfile::tempdir().unwrap();
    let inst = Installed::scan(&tmp.path().join("nope"), &tmp.path().join("nada"));
    assert!(inst.items.is_empty());
    let home = tmp.path().join("h");
    fs::create_dir_all(home.join(".claude/plugins")).unwrap();
    fs::write(
        home.join(".claude/plugins/installed_plugins.json"),
        "{not json",
    )
    .unwrap();
    assert!(Installed::scan(&home, tmp.path()).items.is_empty());
}

#[test]
fn inventory_strings_sorted() {
    let (tmp, home) = home();
    let s = Installed::scan(&home, &project(&tmp)).inventory_strings();
    let mut sorted = s.clone();
    sorted.sort();
    assert_eq!(s, sorted);
    assert!(s.contains(&"skill:superpowers:brainstorming@7.0.2-22f0b463e7a5".to_string()));
    assert!(s.contains(&"agent:helper@-".to_string()));
}

#[test]
fn apm_version_when_lockfile_present() {
    let (tmp, home) = home();
    let inst = Installed::scan(&home, &project(&tmp));
    let a = inst.skill("apm-skill").unwrap();
    assert_eq!(a.version.as_deref(), Some("0.5.0"));
    assert_eq!(a.plugin.as_deref(), Some("smos-demo"));
    assert_eq!(inst.skill("local-skill").unwrap().version, None);
}
