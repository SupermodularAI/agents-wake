//! Who is running: email from Claude Code or git, and the salted pseudonym.

use crate::config::{Config, IdentityMode};
use crate::event::{is_email, UserId};
use crate::repo::run_with_timeout;
use hmac::{Hmac, Mac};
use sha2::Sha256;
use std::path::Path;
use std::process::Command;
use std::time::Duration;

/// `~/.claude.json` `oauthAccount.emailAddress`, else `git config user.email` run in `cwd`.
/// The result must pass `is_email`.
pub fn email(home: &Path, cwd: &Path) -> Option<String> {
    from_claude_json(home)
        .or_else(|| from_git(cwd))
        .map(|e| e.trim().to_string())
        .filter(|e| is_email(e))
}

fn from_claude_json(home: &Path) -> Option<String> {
    let text = std::fs::read_to_string(home.join(".claude.json")).ok()?;
    let v: serde_json::Value = serde_json::from_str(&text).ok()?;
    let e = v.get("oauthAccount")?.get("emailAddress")?.as_str()?.trim();
    is_email(e).then(|| e.to_string())
}

fn from_git(cwd: &Path) -> Option<String> {
    let mut cmd = Command::new("git");
    cmd.args(["config", "user.email"]).current_dir(cwd);
    run_with_timeout(&mut cmd, Duration::from_secs(1))
}

/// `Email(e)`, or `Hash(hex(HMAC-SHA256(salt, lowercase(e))))` in anonymous mode.
/// Anonymous mode without a salt (or without an email) is `Unknown`, never an unsalted hash.
pub fn user_id(cfg: &Config, email: Option<String>) -> UserId {
    let Some(email) = email.map(|e| e.trim().to_string()).filter(|e| is_email(e)) else {
        return UserId::Unknown;
    };
    match cfg.identity {
        IdentityMode::Email => UserId::Email(email),
        IdentityMode::Anonymous => match cfg.identity_salt.as_deref().filter(|s| !s.is_empty()) {
            None => UserId::Unknown,
            Some(salt) => {
                let Ok(mut mac) = Hmac::<Sha256>::new_from_slice(salt.as_bytes()) else {
                    return UserId::Unknown;
                };
                mac.update(email.to_lowercase().as_bytes());
                let hex = mac
                    .finalize()
                    .into_bytes()
                    .iter()
                    .map(|b| format!("{b:02x}"))
                    .collect();
                UserId::Hash(hex)
            }
        },
    }
}
