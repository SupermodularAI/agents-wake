//! Repo name from the `origin` remote: `host/group/name`, never a local path.

use crate::event::is_repo;
use std::io::Read;
use std::path::Path;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

const GIT_TIMEOUT: Duration = Duration::from_secs(1);

/// Run `cmd` with stdin and stderr discarded, kill it after `timeout`, and return trimmed stdout.
/// A spawn failure, a timeout or a non-zero exit gives `None`.
pub(crate) fn run_with_timeout(cmd: &mut Command, timeout: Duration) -> Option<String> {
    let mut child = cmd
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .ok()?;
    let start = Instant::now();
    let status = loop {
        match child.try_wait() {
            Ok(Some(s)) => break s,
            Ok(None) if start.elapsed() < timeout => std::thread::sleep(Duration::from_millis(5)),
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                return None;
            }
        }
    };
    if !status.success() {
        return None;
    }
    let mut out = String::new();
    child.stdout.take()?.read_to_string(&mut out).ok()?;
    Some(out.trim().to_string())
}

/// `git -C cwd remote get-url origin`, normalised.
pub fn repo(cwd: &Path) -> Option<String> {
    let mut cmd = Command::new("git");
    cmd.arg("-C").arg(cwd).args(["remote", "get-url", "origin"]);
    normalise_remote(&run_with_timeout(&mut cmd, GIT_TIMEOUT)?)
}

/// Turn an scp-style, `ssh://` or `https://` remote into `host/group/name`.
/// Drops credentials, port, `.git` and a trailing `/`; lowercases the host. Local paths give `None`.
pub fn normalise_remote(url: &str) -> Option<String> {
    let url = url.trim();
    let (authority, path) = match url.split_once("://") {
        Some((scheme, rest)) => {
            if !matches!(scheme, "ssh" | "https" | "http" | "git") {
                return None;
            }
            rest.split_once('/')?
        }
        None => {
            // scp form: user@host:path (the colon must come before any slash)
            let (host, path) = url.split_once(':')?;
            if host.contains('/') {
                return None;
            }
            (host, path)
        }
    };
    let host = authority.rsplit('@').next()?;
    let host = match (url.contains("://"), host.rsplit_once(':')) {
        (true, Some((h, port))) if port.bytes().all(|c| c.is_ascii_digit()) => h,
        _ => host,
    };
    let path = path.trim_matches('/');
    let path = path.strip_suffix(".git").unwrap_or(path);
    let out = format!("{}/{}", host.to_ascii_lowercase(), path);
    is_repo(&out).then_some(out)
}
