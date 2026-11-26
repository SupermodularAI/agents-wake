//! `wake install` / `wake uninstall` for the open-source path: the user config and the Claude Code hook groups in
//! `~/.claude/settings.json`.
//!
//! `settings.json` is round-tripped in Claude Code's own format (`JSON.stringify(obj, null, 2)`): key order is kept
//! (`serde_json`'s `preserve_order`), indentation is two spaces, and a trailing newline is kept only if it was there.

use crate::config::{self, Env};
use serde_json::{Map, Value};
use std::fs;
use std::io::{self, Write};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};

const HARNESS: &str = "claude-code";

/// The hook events wake installs: (settings key, `wake emit` argument).
const EVENTS: [(&str, &str); 3] = [
    ("SessionStart", "session-start"),
    ("Stop", "stop"),
    ("SessionEnd", "session-end"),
];

pub struct InstallArgs {
    pub endpoint: Option<String>,
    pub headers: Vec<(String, String)>,
    pub identity: Option<String>,
    pub harness: String,
}

/// Asks the user for a missing value. `None` means no answer (not a terminal, or end of input).
pub trait Prompter {
    fn ask(&mut self, q: &str, secret: bool) -> Option<String>;
}

#[derive(Debug)]
pub enum InstallError {
    /// Not valid JSON (comments, trailing commas, not UTF-8) or unreadable.
    SettingsUnreadable,
    /// Valid JSON, but the top level, `hooks`, or one of wake's event entries has the wrong type.
    SettingsNotObject,
    SettingsIsSymlink,
    UnsupportedHarness,
    Io(io::Error),
}

impl std::fmt::Display for InstallError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::SettingsUnreadable => {
                f.write_str("~/.claude/settings.json is not plain JSON (comments or trailing commas?); nothing changed")
            }
            Self::SettingsNotObject => f.write_str(
                "~/.claude/settings.json does not have the expected shape (an object, with `hooks` an object of arrays); nothing changed",
            ),
            Self::SettingsIsSymlink => {
                f.write_str("~/.claude/settings.json is a symlink; edit its target by hand; nothing changed")
            }
            Self::UnsupportedHarness => f.write_str("unsupported harness (only claude-code)"),
            Self::Io(e) => write!(f, "{e}"),
        }
    }
}

impl From<io::Error> for InstallError {
    fn from(e: io::Error) -> Self {
        Self::Io(e)
    }
}

/// Reads answers from the terminal. Asks only when stdin is a TTY; secrets are read with echo off.
pub struct TerminalPrompter;

impl Prompter for TerminalPrompter {
    fn ask(&mut self, q: &str, secret: bool) -> Option<String> {
        use std::io::IsTerminal;
        if !io::stdin().is_terminal() {
            return None;
        }
        let mut err = io::stderr();
        let _ = write!(err, "{q}: ");
        let _ = err.flush();
        let stty = |arg: &str| {
            let _ = std::process::Command::new("stty").arg(arg).status();
        };
        if secret {
            stty("-echo");
        }
        let mut line = String::new();
        let read = io::stdin().read_line(&mut line);
        if secret {
            stty("echo");
            let _ = writeln!(err);
        }
        match read {
            Ok(0) | Err(_) => None,
            Ok(_) => Some(line.trim().to_string()),
        }
    }
}

fn settings_path(home: &Path) -> PathBuf {
    home.join(".claude").join("settings.json")
}

fn hook_command(exe: &Path, event: &str) -> String {
    format!("\"{}\" emit {event}", exe.display())
}

/// The program token of a hook command: inside one pair of double quotes, else up to the first space.
fn program_of(command: &str) -> &str {
    match command.strip_prefix('"') {
        Some(rest) => rest.split('"').next().unwrap_or(""),
        None => command.split(' ').next().unwrap_or(""),
    }
}

/// Whether `group` is one wake installed for `event`: exactly one hook, whose command is this exe's, or an older
/// `…/wake emit <event>`. Never an APM-installed command (under `/.claude/hooks/` or `${PLUGIN_ROOT}`).
fn is_wake_group(group: &Value, exe: &Path, event: &str) -> bool {
    let Some([hook]) = group
        .get("hooks")
        .and_then(Value::as_array)
        .map(Vec::as_slice)
    else {
        return false;
    };
    let Some(command) = hook.get("command").and_then(Value::as_str) else {
        return false;
    };
    let program = program_of(command);
    if program.contains("/.claude/hooks/") || program.contains("${PLUGIN_ROOT}") {
        return false;
    }
    if command == hook_command(exe, event) {
        return true;
    }
    command.ends_with(&format!(" emit {event}"))
        && Path::new(program).file_name().is_some_and(|n| n == "wake")
}

fn wake_group(exe: &Path, event: &str) -> Value {
    serde_json::json!({
        "hooks": [{ "type": "command", "async": true, "command": hook_command(exe, event) }]
    })
}

/// A parsed settings file and what is needed to write it back in the same format.
struct Settings {
    path: PathBuf,
    root: Map<String, Value>,
    exists: bool,
    trailing_newline: bool,
    /// The original file held only ASCII, so non-ASCII characters were written as `\uXXXX` escapes.
    ascii_escaped: bool,
}

/// Loads and validates `settings.json` without changing anything. A missing file is an empty object.
fn load_settings(home: &Path) -> Result<Settings, InstallError> {
    let path = settings_path(home);
    match fs::symlink_metadata(&path) {
        Ok(m) if m.file_type().is_symlink() => return Err(InstallError::SettingsIsSymlink),
        Ok(_) => {}
        Err(e) if e.kind() == io::ErrorKind::NotFound => {
            return Ok(Settings {
                path,
                root: Map::new(),
                exists: false,
                trailing_newline: true,
                ascii_escaped: false,
            })
        }
        Err(e) => return Err(e.into()),
    }
    let bytes = fs::read(&path).map_err(|_| InstallError::SettingsUnreadable)?;
    let value: Value =
        serde_json::from_slice(&bytes).map_err(|_| InstallError::SettingsUnreadable)?;
    let Value::Object(root) = value else {
        return Err(InstallError::SettingsNotObject);
    };
    // wake only edits `hooks.<event>` arrays; anything else there is a shape it will not guess at.
    if let Some(hooks) = root.get("hooks") {
        let hooks = hooks.as_object().ok_or(InstallError::SettingsNotObject)?;
        for (key, _) in EVENTS {
            if hooks.get(key).is_some_and(|v| !v.is_array()) {
                return Err(InstallError::SettingsNotObject);
            }
        }
    }
    Ok(Settings {
        path,
        root,
        exists: true,
        trailing_newline: bytes.ends_with(b"\n"),
        ascii_escaped: bytes.is_ascii(),
    })
}

/// Removes wake's groups from each event array. Returns how many were removed. With `drop_emptied`, an event
/// key that the removal emptied is dropped, and then `hooks` too if it is left empty.
fn remove_wake_groups(root: &mut Map<String, Value>, exe: &Path, drop_emptied: bool) -> usize {
    let Some(Value::Object(hooks)) = root.get_mut("hooks") else {
        return 0;
    };
    let mut removed = 0;
    for (key, event) in EVENTS {
        let Some(Value::Array(groups)) = hooks.get_mut(key) else {
            continue;
        };
        let before = groups.len();
        groups.retain(|g| !is_wake_group(g, exe, event));
        let n = before - groups.len();
        removed += n;
        if drop_emptied && n > 0 && groups.is_empty() {
            hooks.shift_remove(key);
        }
    }
    if drop_emptied && removed > 0 && hooks.is_empty() {
        root.shift_remove("hooks");
    }
    removed
}

/// Pretty formatter that escapes every non-ASCII character as lowercase `\uXXXX` (UTF-16 surrogate pairs).
struct AsciiFormatter(serde_json::ser::PrettyFormatter<'static>);

impl serde_json::ser::Formatter for AsciiFormatter {
    fn begin_array<W: ?Sized + io::Write>(&mut self, w: &mut W) -> io::Result<()> {
        self.0.begin_array(w)
    }
    fn end_array<W: ?Sized + io::Write>(&mut self, w: &mut W) -> io::Result<()> {
        self.0.end_array(w)
    }
    fn begin_array_value<W: ?Sized + io::Write>(
        &mut self,
        w: &mut W,
        first: bool,
    ) -> io::Result<()> {
        self.0.begin_array_value(w, first)
    }
    fn end_array_value<W: ?Sized + io::Write>(&mut self, w: &mut W) -> io::Result<()> {
        self.0.end_array_value(w)
    }
    fn begin_object<W: ?Sized + io::Write>(&mut self, w: &mut W) -> io::Result<()> {
        self.0.begin_object(w)
    }
    fn end_object<W: ?Sized + io::Write>(&mut self, w: &mut W) -> io::Result<()> {
        self.0.end_object(w)
    }
    fn begin_object_key<W: ?Sized + io::Write>(
        &mut self,
        w: &mut W,
        first: bool,
    ) -> io::Result<()> {
        self.0.begin_object_key(w, first)
    }
    fn begin_object_value<W: ?Sized + io::Write>(&mut self, w: &mut W) -> io::Result<()> {
        self.0.begin_object_value(w)
    }
    fn end_object_value<W: ?Sized + io::Write>(&mut self, w: &mut W) -> io::Result<()> {
        self.0.end_object_value(w)
    }
    fn write_string_fragment<W: ?Sized + io::Write>(
        &mut self,
        w: &mut W,
        fragment: &str,
    ) -> io::Result<()> {
        let mut start = 0;
        for (i, c) in fragment.char_indices() {
            if c.is_ascii() {
                continue;
            }
            w.write_all(&fragment.as_bytes()[start..i])?;
            let mut buf = [0u16; 2];
            for unit in c.encode_utf16(&mut buf) {
                write!(w, "\\u{unit:04x}")?;
            }
            start = i + c.len_utf8();
        }
        w.write_all(&fragment.as_bytes()[start..])
    }
}

/// Serialises in Claude Code's format: 2-space indent; non-ASCII raw unless `ascii` asks for escapes.
fn to_pretty(value: &Value, trailing_newline: bool, ascii: bool) -> Vec<u8> {
    let mut out = Vec::new();
    let fmt = serde_json::ser::PrettyFormatter::with_indent(b"  ");
    if ascii {
        let mut ser = serde_json::Serializer::with_formatter(&mut out, AsciiFormatter(fmt));
        serde::Serialize::serialize(value, &mut ser).expect("a Value always serialises");
    } else {
        let mut ser = serde_json::Serializer::with_formatter(&mut out, fmt);
        serde::Serialize::serialize(value, &mut ser).expect("a Value always serialises");
    }
    if trailing_newline {
        out.push(b'\n');
    }
    out
}

/// Writes `bytes` to a temp file beside `path` with `mode`, then renames it over `path`.
fn write_atomic(path: &Path, bytes: &[u8], mode: u32) -> io::Result<()> {
    let name = path.file_name().and_then(|n| n.to_str()).unwrap_or("file");
    let tmp = path.with_file_name(format!(".{name}.wake-tmp-{}", std::process::id()));
    let result = (|| {
        let mut f = fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(mode)
            .open(&tmp)?;
        // `mode` above is filtered by the umask; set it exactly.
        f.set_permissions(fs::Permissions::from_mode(mode))?;
        f.write_all(bytes)?;
        f.sync_all()?;
        fs::rename(&tmp, path)
    })();
    if result.is_err() {
        let _ = fs::remove_file(&tmp);
    }
    result
}

fn write_settings(s: &Settings) -> io::Result<()> {
    if let Some(dir) = s.path.parent() {
        fs::create_dir_all(dir)?;
    }
    let mode = match fs::metadata(&s.path) {
        Ok(m) => m.permissions().mode() & 0o7777,
        Err(_) => 0o644,
    };
    let bytes = to_pretty(
        &Value::Object(s.root.clone()),
        s.trailing_newline,
        s.ascii_escaped,
    );
    write_atomic(&s.path, &bytes, mode)
}

/// Loads the existing user config as a JSON object, so keys wake does not set are kept. Missing → empty.
fn load_config(path: &Path) -> Result<Map<String, Value>, InstallError> {
    match fs::read(path) {
        Ok(bytes) => match serde_json::from_slice(&bytes) {
            Ok(Value::Object(m)) => Ok(m),
            _ => Err(InstallError::Io(io::Error::new(
                io::ErrorKind::InvalidData,
                format!(
                    "{} is not a JSON object; fix or remove it; nothing changed",
                    path.display()
                ),
            ))),
        },
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(Map::new()),
        Err(e) => Err(e.into()),
    }
}

fn non_empty(s: Option<String>) -> Option<String> {
    s.map(|s| s.trim().to_string()).filter(|s| !s.is_empty())
}

/// Fills the config from the args, prompting for values that are neither given nor already configured.
fn apply_args(cfg: &mut Map<String, Value>, args: &InstallArgs, prompt: &mut dyn Prompter) {
    let endpoint = non_empty(args.endpoint.clone()).or_else(|| {
        if cfg.contains_key("endpoint") {
            None
        } else {
            non_empty(prompt.ask("OTLP endpoint URL (empty to skip)", false))
        }
    });
    if let Some(e) = endpoint {
        cfg.insert("endpoint".into(), Value::String(e));
    }

    let mut headers = args.headers.clone();
    if headers.is_empty() && !cfg.contains_key("headers") {
        let answer = non_empty(prompt.ask(
            "Header for the endpoint, as Name=Value, e.g. Authorization=Basic … (empty to skip)",
            true,
        ));
        if let Some((k, v)) = answer.as_deref().and_then(|a| a.split_once('=')) {
            if !k.trim().is_empty() {
                headers.push((k.trim().to_string(), v.trim().to_string()));
            }
        }
    }
    if !headers.is_empty() {
        let map = cfg
            .entry("headers")
            .or_insert_with(|| Value::Object(Map::new()));
        if !map.is_object() {
            *map = Value::Object(Map::new());
        }
        let map = map.as_object_mut().expect("just made an object");
        for (k, v) in headers {
            map.insert(k, Value::String(v));
        }
    }

    let identity = non_empty(args.identity.clone()).or_else(|| {
        if cfg.contains_key("identity") {
            None
        } else {
            non_empty(prompt.ask("Identity mode, email or anonymous (empty for email)", false))
        }
    });
    if let Some(i) = identity.filter(|i| i == "email" || i == "anonymous") {
        cfg.insert("identity".into(), Value::String(i));
    }
}

fn write_config(path: &Path, cfg: &Map<String, Value>) -> io::Result<()> {
    if let Some(dir) = path.parent() {
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(dir)?;
        fs::set_permissions(dir, fs::Permissions::from_mode(0o700))?;
    }
    write_atomic(
        path,
        &to_pretty(&Value::Object(cfg.clone()), true, false),
        0o600,
    )
}

fn config_summary(cfg: &Map<String, Value>) -> String {
    let mut out = String::new();
    match cfg.get("endpoint").and_then(Value::as_str) {
        Some(e) if config::valid_endpoint(e) => {
            out += &format!("  endpoint: {}\n", config::origin(e).unwrap_or_default())
        }
        Some(_) => out += "  endpoint: set, but ignored: it must be https:// (or http:// to localhost)\n",
        None => {
            out += "  endpoint: no endpoint set; wake sends nothing until one is configured (re-run with --endpoint)\n"
        }
    }
    let names: Vec<&str> = cfg
        .get("headers")
        .and_then(Value::as_object)
        .map(|m| m.keys().map(String::as_str).collect())
        .unwrap_or_default();
    if names.is_empty() {
        out += "  headers: none\n";
    } else {
        out += &format!("  headers: {} (values not shown)\n", names.join(", "));
    }
    let identity = cfg
        .get("identity")
        .and_then(Value::as_str)
        .unwrap_or("email (default)");
    out += &format!("  identity: {identity}\n");
    out
}

/// Writes the user config and merges wake's three hook groups into `~/.claude/settings.json`.
///
/// Everything is validated before anything is written: if `settings.json` (or an existing config) is refused,
/// nothing changes. Returns a human summary that never contains header values.
pub fn install(
    env: &dyn Env,
    home: &Path,
    exe: &Path,
    args: &InstallArgs,
    prompt: &mut dyn Prompter,
) -> Result<String, InstallError> {
    if args.harness != HARNESS {
        return Err(InstallError::UnsupportedHarness);
    }
    let mut settings = load_settings(home)?;
    let cfg_path = config::user_config_path(env, home);
    let mut cfg = load_config(&cfg_path)?;

    apply_args(&mut cfg, args, prompt);

    let replaced = remove_wake_groups(&mut settings.root, exe, false);
    let hooks = settings
        .root
        .entry("hooks")
        .or_insert_with(|| Value::Object(Map::new()))
        .as_object_mut()
        .expect("validated by load_settings");
    for (key, event) in EVENTS {
        hooks
            .entry(key)
            .or_insert_with(|| Value::Array(vec![]))
            .as_array_mut()
            .expect("validated by load_settings")
            .push(wake_group(exe, event));
    }

    write_config(&cfg_path, &cfg)?;
    write_settings(&settings)?;

    let mut out = format!(
        "wake installed for Claude Code.\n\nConfig: {}\n",
        cfg_path.display()
    );
    out += &config_summary(&cfg);
    out += &format!(
        "\nHooks: {} {} SessionStart, Stop and SessionEnd → {}\n",
        settings.path.display(),
        if replaced > 0 { "updated" } else { "now runs" },
        exe.display()
    );
    out += "\nRun `wake check` to send a test event.\n";
    Ok(out)
}

/// Removes exactly wake's hook groups from `~/.claude/settings.json`; with `purge`, also the user config dir and
/// the state dir. Never removes the binary. Writes nothing when there is nothing of wake's to remove.
pub fn uninstall(
    env: &dyn Env,
    home: &Path,
    exe: &Path,
    purge: bool,
) -> Result<String, InstallError> {
    let mut settings = load_settings(home)?;
    let removed = if settings.exists {
        remove_wake_groups(&mut settings.root, exe, true)
    } else {
        0
    };
    if removed > 0 {
        write_settings(&settings)?;
    }
    let mut out = if removed > 0 {
        format!(
            "Removed {removed} wake hook group(s) from {}.\n",
            settings.path.display()
        )
    } else {
        format!("No wake hooks found in {}.\n", settings.path.display())
    };

    if purge {
        let cfg_dir = config::user_config_path(env, home)
            .parent()
            .map(Path::to_path_buf)
            .unwrap_or_default();
        for dir in [cfg_dir, crate::state::state_dir(env, home)] {
            match fs::remove_dir_all(&dir) {
                Ok(()) => out += &format!("Removed {}.\n", dir.display()),
                Err(e) if e.kind() == io::ErrorKind::NotFound => {}
                Err(e) => return Err(e.into()),
            }
        }
    }
    out += &format!("The binary is left at {}.\n", exe.display());
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn program_token_parsing() {
        assert_eq!(program_of("\"/a b/wake\" emit stop"), "/a b/wake");
        assert_eq!(program_of("/old/wake emit stop"), "/old/wake");
        assert_eq!(program_of("wake"), "wake");
    }

    #[test]
    fn ownership() {
        let exe = Path::new("/new/bin/wake");
        let g = |cmd: &str| serde_json::json!({"hooks": [{"type": "command", "command": cmd}]});
        assert!(is_wake_group(
            &g("\"/new/bin/wake\" emit stop"),
            exe,
            "stop"
        ));
        assert!(is_wake_group(&g("/old/wake emit stop"), exe, "stop"));
        assert!(!is_wake_group(
            &g("/old/wake emit stop"),
            exe,
            "session-end"
        ));
        assert!(!is_wake_group(&g("/old/notwake emit stop"), exe, "stop"));
        assert!(!is_wake_group(
            &g("\"$HOME/.claude/hooks/p/bin/wake\" emit stop"),
            exe,
            "stop"
        ));
        assert!(!is_wake_group(
            &g("${PLUGIN_ROOT}/bin/wake emit stop"),
            exe,
            "stop"
        ));
        let two = serde_json::json!({"hooks": [
            {"type": "command", "command": "\"/new/bin/wake\" emit stop"},
            {"type": "command", "command": "say hi"}
        ]});
        assert!(!is_wake_group(&two, exe, "stop"));
    }

    #[test]
    fn pretty_prints_empties_like_json_stringify() {
        let v: Value = serde_json::from_str(r#"{"a":[],"b":{},"c":[1],"d":"é"}"#).unwrap();
        let s = String::from_utf8(to_pretty(&v, false, false)).unwrap();
        assert_eq!(
            s,
            "{\n  \"a\": [],\n  \"b\": {},\n  \"c\": [\n    1\n  ],\n  \"d\": \"é\"\n}"
        );
    }
}
