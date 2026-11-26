use std::collections::HashMap;
use std::fs;
use std::path::Path;
use wake::config::*;

struct MapEnv(HashMap<String, String>);
impl Env for MapEnv {
    fn var(&self, key: &str) -> Option<String> {
        self.0.get(key).cloned()
    }
}
fn env(pairs: &[(&str, &str)]) -> MapEnv {
    MapEnv(
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect(),
    )
}
fn write_exe(dir: &Path, json: &str) {
    fs::write(dir.join("telemetry.json"), json).unwrap();
}
fn write_user(home: &Path, json: &str) {
    let p = file_path_user(home);
    fs::create_dir_all(p.parent().unwrap()).unwrap();
    fs::write(p, json).unwrap();
}

#[test]
fn env_beats_files() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    write_exe(e.path(), r#"{"endpoint":"https://exe.example/x"}"#);
    write_user(h.path(), r#"{"endpoint":"https://user.example/x"}"#);
    let c = resolve(
        &env(&[("WAKE_OTLP_ENDPOINT", "https://env.example/x")]),
        e.path(),
        h.path(),
    );
    assert_eq!(c.endpoint.as_deref(), Some("https://env.example/x"));
    assert_eq!(c.sources["endpoint"], Source::Env);
    assert_eq!(c.sources["endpoint"].as_str(), "env");
}

#[test]
fn exe_dir_beats_user_file() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    write_exe(e.path(), r#"{"endpoint":"https://exe.example/x"}"#);
    write_user(h.path(), r#"{"endpoint":"https://user.example/x"}"#);
    let c = resolve(&env(&[]), e.path(), h.path());
    assert_eq!(c.endpoint.as_deref(), Some("https://exe.example/x"));
    assert_eq!(c.sources["endpoint"], Source::ExeDir);
    assert_eq!(c.sources["endpoint"].as_str(), "telemetry.json");
}

#[test]
fn user_file_used_when_alone_and_xdg_respected() {
    let (h, e, x) = (
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
    );
    write_user(h.path(), r#"{"endpoint":"https://user.example/x"}"#);
    let c = resolve(&env(&[]), e.path(), h.path());
    assert_eq!(c.sources["endpoint"].as_str(), "config.json");
    fs::create_dir_all(x.path().join("wake")).unwrap();
    fs::write(
        x.path().join("wake/config.json"),
        r#"{"endpoint":"https://xdg.example/x"}"#,
    )
    .unwrap();
    let c = resolve(
        &env(&[("XDG_CONFIG_HOME", x.path().to_str().unwrap())]),
        e.path(),
        h.path(),
    );
    assert_eq!(c.endpoint.as_deref(), Some("https://xdg.example/x"));
}

#[test]
fn defaults_when_nothing_set() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    let c = resolve(&env(&[]), e.path(), h.path());
    assert_eq!(c.endpoint, None);
    assert!(c.headers.is_empty());
    assert_eq!(c.identity, IdentityMode::Email);
    assert!(c.allow_override);
    assert!(!c.disabled);
    assert_eq!(c.sources["identity"], Source::Default);
    assert_eq!(c.sources["headers"], Source::Default);
}

#[test]
fn allow_override_false_ignores_env() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    write_exe(
        e.path(),
        r#"{"endpoint":"https://exe.example/x","allow_override":false}"#,
    );
    let c = resolve(
        &env(&[("WAKE_OTLP_ENDPOINT", "https://env.example/x")]),
        e.path(),
        h.path(),
    );
    assert_eq!(c.endpoint.as_deref(), Some("https://exe.example/x"));
    assert!(!c.allow_override);
}

#[test]
fn disabled_honoured_even_without_override() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    write_exe(e.path(), r#"{"allow_override":false}"#);
    assert!(resolve(&env(&[("WAKE_DISABLED", "1")]), e.path(), h.path()).disabled);
    assert!(resolve(&env(&[("WAKE_DISABLED", "true")]), e.path(), h.path()).disabled);
    assert!(!resolve(&env(&[("WAKE_DISABLED", "0")]), e.path(), h.path()).disabled);
}

#[test]
fn otel_generic_vars_ignored() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    let c = resolve(
        &env(&[("OTEL_EXPORTER_OTLP_ENDPOINT", "https://x.example")]),
        e.path(),
        h.path(),
    );
    assert_eq!(c.endpoint, None);
}

#[test]
fn http_only_for_loopback() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    let ep = |u: &str| resolve(&env(&[("WAKE_OTLP_ENDPOINT", u)]), e.path(), h.path()).endpoint;
    assert_eq!(
        ep("http://127.0.0.1:4318/v1/traces").as_deref(),
        Some("http://127.0.0.1:4318/v1/traces")
    );
    assert!(ep("http://localhost/x").is_some());
    assert!(ep("http://[::1]:4318/x").is_some());
    assert_eq!(ep("http://example.com/x"), None);
    assert_eq!(ep("http://localhost.evil.com/x"), None);
    assert_eq!(ep("http://127.0.0.1@evil.com/x"), None);
    assert_eq!(ep("ftp://localhost/x"), None);
    assert!(ep("https://example.com/x").is_some());
}

#[test]
fn parse_headers_cases() {
    let want = vec![("Authorization".to_string(), "Basic abc==".to_string())];
    assert_eq!(parse_headers("Authorization=Basic%20abc=="), want);
    assert_eq!(parse_headers("Authorization=Basic abc=="), want);
    assert_eq!(parse_headers("a=1,b=2").len(), 2);
    assert_eq!(parse_headers("a=100%").first().unwrap().1, "100%");
    assert_eq!(parse_headers("a=%zz%4").first().unwrap().1, "%zz%4");
}

#[test]
fn invalid_json_file_is_absent() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    write_exe(e.path(), "{");
    write_user(h.path(), r#"{"endpoint":"https://user.example/x"}"#);
    let c = resolve(&env(&[]), e.path(), h.path());
    assert_eq!(c.endpoint.as_deref(), Some("https://user.example/x"));
}

#[test]
fn identity_headers_salt_from_files_and_env() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    write_exe(
        e.path(),
        r#"{"headers":{"X-A":"1"},"identity":"anonymous","identity_salt":"s"}"#,
    );
    let c = resolve(&env(&[]), e.path(), h.path());
    assert_eq!(c.identity, IdentityMode::Anonymous);
    assert_eq!(c.identity_salt.as_deref(), Some("s"));
    assert_eq!(c.headers, vec![("X-A".to_string(), "1".to_string())]);
    assert_eq!(c.sources["headers"], Source::ExeDir);
    let c = resolve(
        &env(&[("WAKE_IDENTITY", "email"), ("WAKE_OTLP_HEADERS", "k=v")]),
        e.path(),
        h.path(),
    );
    assert_eq!(c.identity, IdentityMode::Email);
    assert_eq!(c.sources["identity"], Source::Env);
    assert_eq!(c.headers, vec![("k".to_string(), "v".to_string())]);
    let c = resolve(&env(&[("WAKE_IDENTITY", "bogus")]), e.path(), h.path());
    assert_eq!(c.identity, IdentityMode::Anonymous);
}

fn auth(c: &Config) -> Option<String> {
    c.headers
        .iter()
        .find(|(k, _)| k.eq_ignore_ascii_case("authorization"))
        .map(|(_, v)| v.clone())
}

#[test]
fn base64_known_answers() {
    for (i, o) in [
        ("", ""),
        ("f", "Zg=="),
        ("fo", "Zm8="),
        ("foo", "Zm9v"),
        ("pk-lf-1:sk-lf-2", "cGstbGYtMTpzay1sZi0y"),
    ] {
        assert_eq!(base64_encode(i.as_bytes()), o);
    }
}

#[test]
fn keys_from_file_derive_basic_auth() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    write_user(h.path(), r#"{"public_key":"pk","secret_key":"sk"}"#);
    let c = resolve(&env(&[]), e.path(), h.path());
    assert_eq!(auth(&c).as_deref(), Some("Basic cGs6c2s="));
    assert!(c.basic_from_keys);
    assert_eq!(c.sources["public_key"], Source::UserConfig);
    assert_eq!(c.sources["secret_key"].as_str(), "config.json");
}

#[test]
fn env_keys_override_file_keys_unless_locked() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    write_user(h.path(), r#"{"public_key":"pk","secret_key":"sk"}"#);
    let envs = env(&[("WAKE_PUBLIC_KEY", "a"), ("WAKE_SECRET_KEY", "b")]);
    let c = resolve(&envs, e.path(), h.path());
    assert_eq!(auth(&c).as_deref(), Some("Basic YTpi"));
    assert_eq!(c.sources["public_key"], Source::Env);
    write_user(
        h.path(),
        r#"{"public_key":"pk","secret_key":"sk","allow_override":false}"#,
    );
    let c = resolve(&envs, e.path(), h.path());
    assert_eq!(auth(&c).as_deref(), Some("Basic cGs6c2s="));
    assert_eq!(c.sources["public_key"], Source::UserConfig);
}

#[test]
fn explicit_authorization_header_wins() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    write_user(
        h.path(),
        r#"{"public_key":"pk","secret_key":"sk","headers":{"authorization":"Bearer x"}}"#,
    );
    let c = resolve(&env(&[]), e.path(), h.path());
    assert_eq!(auth(&c).as_deref(), Some("Bearer x"));
    assert_eq!(c.headers.len(), 1);
    assert!(!c.basic_from_keys);
}

#[test]
fn empty_keys_are_absent_and_fall_through() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    write_exe(
        e.path(),
        r#"{"public_key":"","secret_key":"","endpoint":""}"#,
    );
    write_user(
        h.path(),
        r#"{"public_key":"pk","secret_key":"sk","endpoint":"https://u.example/x"}"#,
    );
    let c = resolve(&env(&[("WAKE_PUBLIC_KEY", "")]), e.path(), h.path());
    assert_eq!(auth(&c).as_deref(), Some("Basic cGs6c2s="));
    assert_eq!(c.sources["public_key"], Source::UserConfig);
    assert_eq!(c.sources["endpoint"], Source::UserConfig);
}

#[test]
fn one_key_gives_no_header() {
    let (h, e) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    write_user(h.path(), r#"{"public_key":"pk","secret_key":""}"#);
    let c = resolve(&env(&[]), e.path(), h.path());
    assert!(c.headers.is_empty());
    assert!(!c.basic_from_keys);
}
