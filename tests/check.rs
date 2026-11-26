use std::cell::RefCell;
use std::collections::HashMap;
use std::path::PathBuf;
use std::time::{Duration, UNIX_EPOCH};
use wake::check::run;
use wake::config::Env;
use wake::harness::claude_code::Deps;
use wake::send::{SendError, Transport};

struct MapEnv(HashMap<String, String>);
impl Env for MapEnv {
    fn var(&self, key: &str) -> Option<String> {
        self.0.get(key).cloned()
    }
}

struct FakeTransport {
    result: Result<(), SendError>,
    calls: RefCell<Vec<String>>,
}
impl FakeTransport {
    fn new(result: Result<(), SendError>) -> Self {
        FakeTransport {
            result,
            calls: RefCell::new(vec![]),
        }
    }
}
impl Transport for FakeTransport {
    fn post(&self, url: &str, _h: &[(String, String)], _b: &[u8]) -> Result<(), SendError> {
        self.calls.borrow_mut().push(url.to_string());
        self.result.clone()
    }
}

fn check(pairs: &[(&str, &str)], t: &FakeTransport) -> (i32, String) {
    let dir = std::env::temp_dir().join(format!("wake-check-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let env = MapEnv(
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect(),
    );
    let deps = Deps {
        env: &env,
        home: dir.clone(),
        exe_dir: PathBuf::from(&dir),
        transport: t,
        now: UNIX_EPOCH + Duration::from_secs(1_700_000_000),
    };
    let mut out = Vec::new();
    let code = run(&deps, &mut out);
    (code, String::from_utf8(out).unwrap())
}

#[test]
fn check_never_prints_secrets() {
    let t = FakeTransport::new(Ok(()));
    let (_, out) = check(
        &[
            (
                "WAKE_OTLP_ENDPOINT",
                "https://otel.example.com:4318/v1/traces?key=zzz",
            ),
            ("WAKE_OTLP_HEADERS", "Authorization=Basic c2VjcmV0"),
        ],
        &t,
    );
    for s in ["c2VjcmV0", "Basic", "/v1/traces", "zzz", "Authorization"] {
        assert!(!out.contains(s), "leaked {s}: {out}");
    }
    assert!(out.contains("endpoint: https://otel.example.com:4318 (from env)"));
    assert!(out.contains("headers: 1 set (from env)"));
}

#[test]
fn check_reports_sources() {
    let t = FakeTransport::new(Ok(()));
    let (_, out) = check(&[("WAKE_OTLP_ENDPOINT", "https://o.example.com")], &t);
    assert!(out.contains("(from env)"), "{out}");
    assert!(out.contains("identity: email (from default)"), "{out}");
    assert!(out.contains("disabled: no"), "{out}");
}

#[test]
fn check_send_result() {
    let ep = [("WAKE_OTLP_ENDPOINT", "https://o.example.com/x")];
    let t = FakeTransport::new(Ok(()));
    let (code, out) = check(&ep, &t);
    assert_eq!(code, 0);
    assert!(out.contains("test event: sent"), "{out}");
    assert_eq!(t.calls.borrow().len(), 1);

    let t = FakeTransport::new(Err(SendError::Network));
    let (code, out) = check(&ep, &t);
    assert_eq!(code, 1);
    assert!(out.contains("test event: failed (network)"), "{out}");

    let t = FakeTransport::new(Err(SendError::Status(401)));
    let (code, out) = check(&ep, &t);
    assert_eq!(code, 1);
    assert!(out.contains("test event: failed (http 401)"), "{out}");
}

#[test]
fn check_without_endpoint_or_when_disabled_does_not_send() {
    let t = FakeTransport::new(Ok(()));
    let (code, out) = check(&[], &t);
    assert_eq!(code, 1);
    assert!(out.contains("endpoint: none (from default)"), "{out}");
    assert!(out.contains("test event: skipped (no endpoint)"), "{out}");

    let (code, out) = check(
        &[
            ("WAKE_OTLP_ENDPOINT", "https://o.example.com"),
            ("WAKE_DISABLED", "1"),
        ],
        &t,
    );
    assert_eq!(code, 0);
    assert!(out.contains("disabled: yes"), "{out}");
    assert!(t.calls.borrow().is_empty());
}
