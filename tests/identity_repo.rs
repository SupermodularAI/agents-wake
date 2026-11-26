use std::path::Path;
use wake::config::{Config, IdentityMode};
use wake::event::{is_repo, UserId};
use wake::identity::{email, user_id};
use wake::repo::{normalise_remote, repo};

fn cfg(mode: IdentityMode, salt: Option<&str>) -> Config {
    Config {
        endpoint: None,
        headers: vec![],
        identity: mode,
        identity_salt: salt.map(String::from),
        allow_override: true,
        basic_from_keys: false,
        disabled: false,
        sources: Default::default(),
    }
}

#[test]
fn normalise_ssh() {
    assert_eq!(
        normalise_remote("git@gitlab.com:supermodularai/core/agents-wake.git").as_deref(),
        Some("gitlab.com/supermodularai/core/agents-wake")
    );
}

#[test]
fn normalise_https_with_creds() {
    let r = normalise_remote("https://oauth2:tok@github.com/SupermodularAI/agents-wake").unwrap();
    assert_eq!(r, "github.com/SupermodularAI/agents-wake");
    assert!(!r.contains("tok"));
}

#[test]
fn normalise_ssh_url_port() {
    assert_eq!(
        normalise_remote("ssh://git@host.example:2222/a/b.git").as_deref(),
        Some("host.example/a/b")
    );
}

#[test]
fn normalise_host_lowercased_trailing_slash_stripped() {
    assert_eq!(
        normalise_remote("https://GitHub.COM/Org/Repo.git/").as_deref(),
        Some("github.com/Org/Repo")
    );
}

#[test]
fn normalise_local_path_rejected() {
    assert_eq!(normalise_remote("/Users/x/repo.git"), None);
    assert_eq!(normalise_remote("file:///x"), None);
    assert_eq!(normalise_remote("../x/y"), None);
    assert_eq!(normalise_remote(""), None);
}

#[test]
fn email_from_claude_json() {
    let home = tempfile::tempdir().unwrap();
    std::fs::write(
        home.path().join(".claude.json"),
        r#"{"oauthAccount":{"emailAddress":"a@b.co"}}"#,
    )
    .unwrap();
    assert_eq!(
        email(home.path(), Path::new("/nonexistent")).as_deref(),
        Some("a@b.co")
    );
}

#[test]
fn anonymous_hash_is_stable_and_salted() {
    let a = user_id(
        &cfg(IdentityMode::Anonymous, Some("s1")),
        Some("A@B.co".into()),
    );
    let b = user_id(
        &cfg(IdentityMode::Anonymous, Some("s1")),
        Some(" a@b.co ".into()),
    );
    let c = user_id(
        &cfg(IdentityMode::Anonymous, Some("s2")),
        Some("a@b.co".into()),
    );
    let (UserId::Hash(a), UserId::Hash(b), UserId::Hash(c)) = (a, b, c) else {
        panic!("expected hashes");
    };
    assert_eq!(a.len(), 64);
    assert!(a
        .bytes()
        .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c)));
    assert_eq!(a, b);
    assert_ne!(a, c);
    assert!(!a.contains('@'));
}

#[test]
fn anonymous_without_salt_is_unknown() {
    assert_eq!(
        user_id(&cfg(IdentityMode::Anonymous, None), Some("a@b.co".into())),
        UserId::Unknown
    );
}

#[test]
fn email_mode_passes_email_through() {
    assert_eq!(
        user_id(&cfg(IdentityMode::Email, None), Some("a@b.co".into())),
        UserId::Email("a@b.co".into())
    );
    assert_eq!(
        user_id(&cfg(IdentityMode::Email, None), None),
        UserId::Unknown
    );
}

#[test]
fn repo_reads_origin_of_a_git_checkout() {
    let git_ok = std::process::Command::new("git")
        .arg("--version")
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false);
    if !git_ok {
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    let run = |args: &[&str]| {
        std::process::Command::new("git")
            .args(args)
            .current_dir(dir.path())
            .output()
            .unwrap()
    };
    run(&["init", "-q"]);
    run(&["config", "remote.origin.url", "git@gitlab.com:g/n.git"]);
    let r = repo(dir.path()).unwrap();
    assert_eq!(r, "gitlab.com/g/n");
    assert!(is_repo(&r));
}
