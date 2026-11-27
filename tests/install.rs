use serde_json::Value;
use std::collections::HashMap;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use wake::config::Env;
use wake::install::{install, uninstall, InstallArgs, InstallError, Prompter};

struct MapEnv(HashMap<String, String>);
impl Env for MapEnv {
    fn var(&self, key: &str) -> Option<String> {
        self.0.get(key).cloned()
    }
}

fn no_env() -> MapEnv {
    MapEnv(HashMap::new())
}

/// Answers nothing, like a non-terminal stdin.
struct Silent;
impl Prompter for Silent {
    fn ask(&mut self, _q: &str, _secret: bool) -> Option<String> {
        None
    }
}

/// Answers from a script, recording whether each question was secret.
struct Scripted {
    answers: Vec<String>,
    asked: Vec<(String, bool)>,
}
impl Prompter for Scripted {
    fn ask(&mut self, q: &str, secret: bool) -> Option<String> {
        self.asked.push((q.to_string(), secret));
        if self.answers.is_empty() {
            None
        } else {
            Some(self.answers.remove(0))
        }
    }
}

struct Home {
    _dir: tempfile::TempDir,
    home: PathBuf,
    exe: PathBuf,
}

fn home() -> Home {
    let dir = tempfile::tempdir().unwrap();
    let home = dir.path().join("home");
    std::fs::create_dir_all(&home).unwrap();
    let exe = dir.path().join("bin").join("wake");
    Home {
        _dir: dir,
        home,
        exe,
    }
}

fn settings_path(home: &Path) -> PathBuf {
    home.join(".claude").join("settings.json")
}

fn write_settings(home: &Path, text: &str) {
    let p = settings_path(home);
    std::fs::create_dir_all(p.parent().unwrap()).unwrap();
    std::fs::write(p, text).unwrap();
}

fn read_settings(home: &Path) -> Value {
    serde_json::from_slice(&std::fs::read(settings_path(home)).unwrap()).unwrap()
}

fn args() -> InstallArgs {
    InstallArgs {
        endpoint: Some("https://lf.example.com/api/public/otel/v1/traces".into()),
        public_key: None,
        secret_key: None,
        headers: vec![],
        identity: None,
        harness: "claude-code".into(),
    }
}

const EVENTS: [(&str, &str); 3] = [
    ("SessionStart", "session-start"),
    ("Stop", "stop"),
    ("SessionEnd", "session-end"),
];

/// Commands of the groups wake owns (exact current-exe match) under each event.
fn wake_groups(settings: &Value, exe: &Path) -> Vec<Value> {
    let mut out = vec![];
    for (key, event) in EVENTS {
        let want = format!("\"{}\" emit {event}", exe.display());
        for g in settings["hooks"][key]
            .as_array()
            .cloned()
            .unwrap_or_default()
        {
            let hooks = g["hooks"].as_array().unwrap();
            if hooks.len() == 1 && hooks[0]["command"] == want.as_str() {
                out.push(g.clone());
            }
        }
    }
    out
}

#[test]
fn install_into_empty() {
    let h = home();
    let summary = install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap();
    assert!(!summary.is_empty());
    let s = read_settings(&h.home);
    let groups = wake_groups(&s, &h.exe);
    assert_eq!(groups.len(), 3);
    for g in &groups {
        let hook = &g["hooks"][0];
        assert_eq!(hook["type"], "command");
        assert_eq!(hook["async"], true);
        let cmd = hook["command"].as_str().unwrap();
        let program = cmd.strip_prefix('"').unwrap().split('"').next().unwrap();
        assert!(Path::new(program).is_absolute(), "{cmd}");
    }
    // A newly created settings file ends with a newline.
    let bytes = std::fs::read(settings_path(&h.home)).unwrap();
    assert!(bytes.ends_with(b"}\n"));
}

const OTHERS: &str = r#"{
  "env": {
    "ZED": "1",
    "ALPHA": "2"
  },
  "permissions": {
    "allow": [
      "Bash(ls:*)"
    ],
    "deny": []
  },
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "say done"
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/guard"
          }
        ]
      }
    ]
  },
  "model": "opus"
}
"#;

#[test]
fn install_preserves_others_and_order() {
    let h = home();
    write_settings(&h.home, OTHERS);
    install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap();
    let after = read_settings(&h.home);
    let before: Value = serde_json::from_str(OTHERS).unwrap();

    // Top-level and nested key order is unchanged.
    let keys = |v: &Value| v.as_object().unwrap().keys().cloned().collect::<Vec<_>>();
    assert_eq!(keys(&after), keys(&before));
    assert_eq!(keys(&after["env"]), vec!["ZED", "ALPHA"]);
    for k in ["env", "permissions", "model"] {
        assert_eq!(after[k], before[k], "{k}");
    }
    assert_eq!(after["hooks"]["PreToolUse"], before["hooks"]["PreToolUse"]);
    // Existing hook keys keep their positions; new ones are appended.
    assert_eq!(
        keys(&after["hooks"]),
        vec!["Stop", "PreToolUse", "SessionStart", "SessionEnd"]
    );
    // The user's Stop group stays first; only wake's is appended.
    let stop = after["hooks"]["Stop"].as_array().unwrap();
    assert_eq!(stop.len(), 2);
    assert_eq!(stop[0], before["hooks"]["Stop"][0]);
    assert_eq!(wake_groups(&after, &h.exe).len(), 3);
}

#[test]
fn install_is_idempotent() {
    let h = home();
    write_settings(&h.home, OTHERS);
    install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap();
    let once = std::fs::read(settings_path(&h.home)).unwrap();
    install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap();
    let twice = std::fs::read(settings_path(&h.home)).unwrap();
    assert_eq!(wake_groups(&read_settings(&h.home), &h.exe).len(), 3);
    assert_eq!(once, twice);
}

#[test]
fn install_replaces_old_exe_path() {
    let h = home();
    write_settings(
        &h.home,
        r#"{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "async": true,
            "command": "/old/wake emit stop"
          }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "\"/older/bin/wake\" emit stop"
          }
        ]
      }
    ]
  }
}
"#,
    );
    install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap();
    let s = read_settings(&h.home);
    let stop = s["hooks"]["Stop"].as_array().unwrap();
    assert_eq!(stop.len(), 1, "{stop:?}");
    assert_eq!(wake_groups(&s, &h.exe).len(), 3);
}

fn assert_refuses(h: &Home, expect: fn(&InstallError) -> bool) {
    let p = settings_path(&h.home);
    let before = std::fs::read(&p).unwrap();
    let err = install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap_err();
    assert!(expect(&err), "{err:?}");
    assert_eq!(std::fs::read(&p).unwrap(), before);
    // Nothing else was written either: no config.json.
    assert!(!h.home.join(".config/wake/config.json").exists());
}

#[test]
fn refuses_comments_and_symlink() {
    let h = home();
    write_settings(&h.home, "{\n  // x\n  \"model\": \"opus\"\n}\n");
    assert_refuses(&h, |e| matches!(e, InstallError::SettingsUnreadable));

    let h = home();
    write_settings(&h.home, "{\n  \"model\": \"opus\",\n}\n");
    assert_refuses(&h, |e| matches!(e, InstallError::SettingsUnreadable));

    let h = home();
    write_settings(&h.home, "[1, 2]\n");
    assert_refuses(&h, |e| matches!(e, InstallError::SettingsNotObject));

    let h = home();
    let real = h.home.join("real-settings.json");
    std::fs::write(&real, "{}\n").unwrap();
    std::fs::create_dir_all(h.home.join(".claude")).unwrap();
    std::os::unix::fs::symlink(&real, settings_path(&h.home)).unwrap();
    assert_refuses(&h, |e| matches!(e, InstallError::SettingsIsSymlink));
    assert_eq!(std::fs::read(&real).unwrap(), b"{}\n");
    assert!(std::fs::symlink_metadata(settings_path(&h.home))
        .unwrap()
        .file_type()
        .is_symlink());
}

#[test]
fn refuses_other_harness() {
    let h = home();
    let mut a = args();
    a.harness = "codex".into();
    let err = install(&no_env(), &h.home, &h.exe, &a, &mut Silent).unwrap_err();
    assert!(matches!(err, InstallError::UnsupportedHarness));
    assert!(!settings_path(&h.home).exists());
    assert!(!h.home.join(".config").exists());
}

#[test]
fn refuses_exe_path_with_shell_metacharacters() {
    // The hook command is run by a shell with the path in double quotes, where these still act.
    for bad in ["a\"b", "a$b", "a`b", "a\\b"] {
        let h = home();
        let exe = h.home.join(bad).join("wake");
        let err = install(&no_env(), &h.home, &exe, &args(), &mut Silent).unwrap_err();
        assert!(matches!(err, InstallError::UnsafeExePath), "{bad}: {err:?}");
        assert!(err.to_string().contains("wake"), "{err}");
        assert!(!settings_path(&h.home).exists(), "{bad}");
        assert!(!h.home.join(".config").exists(), "{bad}");
    }
}

#[test]
fn config_written_0600_without_echoing_secret() {
    let h = home();
    let mut a = args();
    a.headers = vec![("Authorization".into(), "Basic c2VjcmV0LXZhbHVl".into())];
    a.identity = Some("anonymous".into());
    let summary = install(&no_env(), &h.home, &h.exe, &a, &mut Silent).unwrap();
    assert!(!summary.contains("c2VjcmV0LXZhbHVl"), "{summary}");
    assert!(summary.contains("Authorization"), "{summary}");

    let cfg_path = h.home.join(".config/wake/config.json");
    let mode = std::fs::metadata(&cfg_path).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode, 0o600);
    let dir_mode = std::fs::metadata(cfg_path.parent().unwrap())
        .unwrap()
        .permissions()
        .mode()
        & 0o777;
    assert_eq!(dir_mode, 0o700);

    let cfg: Value = serde_json::from_slice(&std::fs::read(&cfg_path).unwrap()).unwrap();
    assert_eq!(
        cfg["endpoint"],
        "https://lf.example.com/api/public/otel/v1/traces"
    );
    assert_eq!(cfg["headers"]["Authorization"], "Basic c2VjcmV0LXZhbHVl");
    assert_eq!(cfg["identity"], "anonymous");
}

#[test]
fn config_keeps_existing_keys_not_given() {
    let h = home();
    let cfg_path = h.home.join(".config/wake/config.json");
    std::fs::create_dir_all(cfg_path.parent().unwrap()).unwrap();
    std::fs::write(
        &cfg_path,
        r#"{"endpoint":"https://old.example.com","headers":{"X-Old":"1"},"identity":"anonymous","identity_salt":"s","allow_override":false}"#,
    )
    .unwrap();
    let mut a = args();
    a.headers = vec![("X-New".into(), "2".into())];
    install(&no_env(), &h.home, &h.exe, &a, &mut Silent).unwrap();
    let cfg: Value = serde_json::from_slice(&std::fs::read(&cfg_path).unwrap()).unwrap();
    assert_eq!(
        cfg["endpoint"],
        "https://lf.example.com/api/public/otel/v1/traces"
    );
    assert_eq!(cfg["headers"]["X-Old"], "1");
    assert_eq!(cfg["headers"]["X-New"], "2");
    assert_eq!(cfg["identity"], "anonymous");
    assert_eq!(cfg["identity_salt"], "s");
    assert_eq!(cfg["allow_override"], false);
}

#[test]
fn config_dir_follows_xdg_config_home() {
    let h = home();
    let xdg = h.home.join("xdg");
    let env = MapEnv(HashMap::from([(
        "XDG_CONFIG_HOME".to_string(),
        xdg.display().to_string(),
    )]));
    install(&env, &h.home, &h.exe, &args(), &mut Silent).unwrap();
    assert!(xdg.join("wake/config.json").exists());
    assert!(!h.home.join(".config").exists());
}

#[test]
fn prompts_for_missing_values_with_secret_hidden() {
    let h = home();
    let mut a = args();
    a.endpoint = None;
    let mut p = Scripted {
        answers: vec![
            "https://prompted.example.com".into(),
            "pk-prompted".into(),
            "sk-PROMPTED".into(),
        ],
        asked: vec![],
    };
    let summary = install(&no_env(), &h.home, &h.exe, &a, &mut p).unwrap();
    assert!(!summary.contains("sk-PROMPTED"), "{summary}");
    assert!(
        summary.contains("keys: public set, secret set"),
        "{summary}"
    );
    // Only the secret key is read without echo.
    assert_eq!(p.asked.len(), 3, "{:?}", p.asked);
    assert!(!p.asked[0].1);
    assert!(!p.asked[1].1);
    assert!(p.asked[2].1);
    let cfg: Value =
        serde_json::from_slice(&std::fs::read(h.home.join(".config/wake/config.json")).unwrap())
            .unwrap();
    assert_eq!(cfg["endpoint"], "https://prompted.example.com");
    assert_eq!(cfg["public_key"], "pk-prompted");
    assert_eq!(cfg["secret_key"], "sk-PROMPTED");
    assert_eq!(cfg["identity"], "email");
}

#[test]
fn no_endpoint_without_terminal_still_installs() {
    let h = home();
    let mut a = args();
    a.endpoint = None;
    let summary = install(&no_env(), &h.home, &h.exe, &a, &mut Silent).unwrap();
    assert!(summary.to_lowercase().contains("no endpoint"), "{summary}");
    assert_eq!(wake_groups(&read_settings(&h.home), &h.exe).len(), 3);
}

/// Formatted the way Claude Code writes settings.json: `JSON.stringify(obj, null, 2)`, non-ASCII unescaped,
/// empty arrays and objects as `[]` / `{}`.
const CLAUDE_FORMATTED: &str = r#"{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "env": {
    "GREETING": "olá — ünïcødé ✓",
    "QUOTE": "say \"hi\"\n\ttab \\ backslash \u001b esc"
  },
  "permissions": {
    "allow": [],
    "deny": [
      "Read(./.env)"
    ],
    "additionalDirectories": []
  },
  "enabledPlugins": {},
  "hooks": {
    "Stop": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "\"$HOME/.claude/hooks/smos-telemetry/bin/wake\" emit stop",
            "async": true
          }
        ]
      }
    ],
    "PreToolUse": []
  },
  "cleanupPeriodDays": 30,
  "ratio": 0.25,
  "includeCoAuthoredBy": false,
  "statusLine": null
}"#;

#[test]
fn uninstall_restores_bytes() {
    for text in [
        format!("{CLAUDE_FORMATTED}\n"),
        CLAUDE_FORMATTED.to_string(),
    ] {
        let h = home();
        write_settings(&h.home, &text);
        install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap();
        let installed = std::fs::read_to_string(settings_path(&h.home)).unwrap();
        assert_ne!(installed, text);
        assert_eq!(installed.ends_with('\n'), text.ends_with('\n'));
        uninstall(&no_env(), &h.home, &h.exe, false).unwrap();
        let after = std::fs::read_to_string(settings_path(&h.home)).unwrap();
        assert_eq!(after, text);
    }
}

#[test]
fn uninstall_drops_keys_it_emptied() {
    let h = home();
    let original = "{\n  \"model\": \"opus\"\n}\n";
    write_settings(&h.home, original);
    install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap();
    uninstall(&no_env(), &h.home, &h.exe, false).unwrap();
    assert_eq!(
        std::fs::read_to_string(settings_path(&h.home)).unwrap(),
        original
    );
}

#[test]
fn uninstall_without_wake_groups_writes_nothing() {
    let h = home();
    // Not Claude Code's format: uninstall must not rewrite it when nothing is wake's.
    let original = "{\"hooks\":{\"Stop\":[]},\"model\":\"opus\"}";
    write_settings(&h.home, original);
    uninstall(&no_env(), &h.home, &h.exe, false).unwrap();
    assert_eq!(
        std::fs::read_to_string(settings_path(&h.home)).unwrap(),
        original
    );
    // A missing settings file stays missing.
    let h = home();
    uninstall(&no_env(), &h.home, &h.exe, false).unwrap();
    assert!(!settings_path(&h.home).exists());
}

#[test]
fn uninstall_keeps_user_edited_group() {
    let h = home();
    install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap();
    let mut s = read_settings(&h.home);
    s["hooks"]["Stop"][0]["hooks"]
        .as_array_mut()
        .unwrap()
        .push(serde_json::json!({"type": "command", "command": "say bye"}));
    std::fs::write(
        settings_path(&h.home),
        serde_json::to_string_pretty(&s).unwrap(),
    )
    .unwrap();
    uninstall(&no_env(), &h.home, &h.exe, false).unwrap();
    let after = read_settings(&h.home);
    let stop = after["hooks"]["Stop"].as_array().unwrap();
    assert_eq!(stop.len(), 1);
    assert_eq!(stop[0]["hooks"].as_array().unwrap().len(), 2);
    assert!(after["hooks"].get("SessionStart").is_none());
    assert!(after["hooks"].get("SessionEnd").is_none());
}

#[test]
fn apm_hooks_untouched() {
    let h = home();
    let apm_group = serde_json::json!({
        "matcher": "*",
        "hooks": [{
            "type": "command",
            "command": "\"$HOME/.claude/hooks/smos-telemetry/bin/wake\" emit stop",
            "async": true
        }]
    });
    let plugin_group = serde_json::json!({
        "hooks": [{"type": "command", "command": "${PLUGIN_ROOT}/bin/wake emit stop"}]
    });
    let s = serde_json::json!({"hooks": {"Stop": [apm_group.clone(), plugin_group.clone()]}});
    write_settings(&h.home, &serde_json::to_string_pretty(&s).unwrap());

    install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap();
    let after = read_settings(&h.home);
    let stop = after["hooks"]["Stop"].as_array().unwrap();
    assert_eq!(stop.len(), 3);
    assert_eq!(stop[0], apm_group);
    assert_eq!(stop[1], plugin_group);
    assert_eq!(wake_groups(&after, &h.exe).len(), 3);

    uninstall(&no_env(), &h.home, &h.exe, false).unwrap();
    let after = read_settings(&h.home);
    assert_eq!(
        after["hooks"]["Stop"],
        serde_json::json!([apm_group, plugin_group])
    );
}

#[test]
fn uninstall_purge_removes_config_and_state_not_binary() {
    let h = home();
    std::fs::create_dir_all(h.exe.parent().unwrap()).unwrap();
    std::fs::write(&h.exe, b"binary").unwrap();
    install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap();
    let state = h.home.join(".local/state/wake/sessions");
    std::fs::create_dir_all(&state).unwrap();
    std::fs::write(state.join("x.json"), "{}").unwrap();

    uninstall(&no_env(), &h.home, &h.exe, true).unwrap();
    assert!(!h.home.join(".config/wake").exists());
    assert!(!h.home.join(".local/state/wake").exists());
    assert!(h.home.join(".local/state").exists());
    assert!(h.exe.exists());
}

#[test]
fn uninstall_purge_honours_xdg_dirs() {
    let h = home();
    let cfg = h.home.join("xdg-config");
    let st = h.home.join("xdg-state");
    let env = MapEnv(HashMap::from([
        ("XDG_CONFIG_HOME".to_string(), cfg.display().to_string()),
        ("XDG_STATE_HOME".to_string(), st.display().to_string()),
    ]));
    install(&env, &h.home, &h.exe, &args(), &mut Silent).unwrap();
    std::fs::create_dir_all(st.join("wake")).unwrap();
    uninstall(&env, &h.home, &h.exe, true).unwrap();
    assert!(!cfg.join("wake").exists());
    assert!(!st.join("wake").exists());
}

#[test]
fn uninstall_refuses_unparsable_settings() {
    let h = home();
    let text = "{ // nope\n}";
    write_settings(&h.home, text);
    let err = uninstall(&no_env(), &h.home, &h.exe, false).unwrap_err();
    assert!(matches!(err, InstallError::SettingsUnreadable), "{err:?}");
    assert_eq!(
        std::fs::read_to_string(settings_path(&h.home)).unwrap(),
        text
    );
}

#[test]
fn uninstall_restores_bytes_ascii_escaped() {
    let text = "{\n  \"note\": \"a \\u2014 b \\ud83d\\ude80 \\u001b\",\n  \"hooks\": {\n    \"PreToolUse\": []\n  },\n  \"theme\": \"dark\"\n}\n";
    assert!(text.is_ascii());
    let h = home();
    write_settings(&h.home, text);
    install(&no_env(), &h.home, &h.exe, &args(), &mut Silent).unwrap();
    let installed = std::fs::read(settings_path(&h.home)).unwrap();
    assert!(installed.is_ascii());
    assert!(String::from_utf8(installed)
        .unwrap()
        .contains("\\u2014 b \\ud83d\\ude80 \\u001b"));
    uninstall(&no_env(), &h.home, &h.exe, false).unwrap();
    assert_eq!(
        std::fs::read_to_string(settings_path(&h.home)).unwrap(),
        text
    );
}

const TEMPLATE_ORDER: [&str; 7] = [
    "endpoint",
    "public_key",
    "secret_key",
    "headers",
    "identity",
    "identity_salt",
    "allow_override",
];

#[test]
fn fresh_install_writes_the_full_template_in_order() {
    let h = home();
    let mut a = args();
    a.endpoint = None;
    let summary = install(&no_env(), &h.home, &h.exe, &a, &mut Silent).unwrap();
    let cfg_path = h.home.join(".config/wake/config.json");
    let mode = std::fs::metadata(&cfg_path).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode, 0o600);
    let text = std::fs::read_to_string(&cfg_path).unwrap();
    assert_eq!(
        text,
        "{\n  \"endpoint\": \"\",\n  \"public_key\": \"\",\n  \"secret_key\": \"\",\n  \"headers\": {},\n  \"identity\": \"email\",\n  \"identity_salt\": \"\",\n  \"allow_override\": true\n}\n"
    );
    let cfg: Value = serde_json::from_str(&text).unwrap();
    let keys: Vec<&String> = cfg.as_object().unwrap().keys().collect();
    assert_eq!(keys, TEMPLATE_ORDER);
    assert!(
        summary.contains("keys: public missing, secret missing"),
        "{summary}"
    );
    let p = cfg_path.display().to_string();
    assert!(
        summary
            .trim_end()
            .ends_with(&format!("Edit {p} to change endpoint, keys or identity.")),
        "{summary}"
    );
}

#[test]
fn key_flags_are_written_and_never_printed() {
    let h = home();
    let mut a = args();
    a.public_key = Some("pk-lf-1".into());
    a.secret_key = Some("sk-lf-TOPSECRET".into());
    let summary = install(&no_env(), &h.home, &h.exe, &a, &mut Silent).unwrap();
    assert!(!summary.contains("TOPSECRET"), "{summary}");
    assert!(
        summary.contains("keys: public set, secret set"),
        "{summary}"
    );
    let cfg: Value =
        serde_json::from_slice(&std::fs::read(h.home.join(".config/wake/config.json")).unwrap())
            .unwrap();
    assert_eq!(cfg["public_key"], "pk-lf-1");
    assert_eq!(cfg["secret_key"], "sk-lf-TOPSECRET");
}

#[test]
fn reinstall_keeps_non_empty_values_and_unknown_keys() {
    let h = home();
    let cfg_path = h.home.join(".config/wake/config.json");
    std::fs::create_dir_all(cfg_path.parent().unwrap()).unwrap();
    std::fs::write(
        &cfg_path,
        r#"{"extra":{"a":1},"secret_key":"sk-old","endpoint":"https://old.example.com","public_key":"","identity":"anonymous","identity_salt":"s"}"#,
    )
    .unwrap();
    let mut a = args();
    a.endpoint = None;
    a.public_key = Some("pk-new".into());
    install(&no_env(), &h.home, &h.exe, &a, &mut Silent).unwrap();
    let cfg: Value = serde_json::from_slice(&std::fs::read(&cfg_path).unwrap()).unwrap();
    assert_eq!(cfg["endpoint"], "https://old.example.com");
    assert_eq!(cfg["public_key"], "pk-new");
    assert_eq!(cfg["secret_key"], "sk-old");
    assert_eq!(cfg["identity"], "anonymous");
    assert_eq!(cfg["identity_salt"], "s");
    assert_eq!(cfg["allow_override"], true);
    assert_eq!(cfg["extra"]["a"], 1);
    let keys: Vec<&String> = cfg.as_object().unwrap().keys().collect();
    assert_eq!(keys[..7], TEMPLATE_ORDER);
    assert_eq!(keys[7], "extra");
}
