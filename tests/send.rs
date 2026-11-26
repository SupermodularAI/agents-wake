use serde_json::{json, Value};
use std::cell::RefCell;
use std::collections::{BTreeMap, VecDeque};
use std::fs;
use std::path::Path;
use std::time::{Duration, Instant};
use wake::config::{Config, IdentityMode};
use wake::send::*;

const NOW: u64 = 10_000_000;
const DAY: u64 = 86_400;

struct FakeTransport {
    calls: RefCell<Vec<Value>>,
    script: RefCell<VecDeque<Result<(), SendError>>>,
    default: Result<(), SendError>,
}
impl FakeTransport {
    fn new(script: Vec<Result<(), SendError>>, default: Result<(), SendError>) -> Self {
        FakeTransport {
            calls: RefCell::new(vec![]),
            script: RefCell::new(script.into()),
            default,
        }
    }
    fn calls(&self) -> Vec<Value> {
        self.calls.borrow().clone()
    }
}
impl Transport for FakeTransport {
    fn post(&self, _url: &str, _h: &[(String, String)], body: &[u8]) -> Result<(), SendError> {
        self.calls
            .borrow_mut()
            .push(serde_json::from_slice(body).unwrap());
        self.script
            .borrow_mut()
            .pop_front()
            .unwrap_or_else(|| self.default.clone())
    }
}

fn cfg(endpoint: Option<&str>) -> Config {
    Config {
        endpoint: endpoint.map(String::from),
        headers: vec![],
        identity: IdentityMode::Anonymous,
        identity_salt: None,
        allow_override: true,
        disabled: false,
        sources: BTreeMap::new(),
    }
}
fn outbox(dir: &Path) -> Outbox {
    Outbox {
        path: dir.join("state").join("outbox.ndjson"),
    }
}
fn read_lines(o: &Outbox) -> Vec<Value> {
    fs::read_to_string(&o.path)
        .unwrap()
        .lines()
        .map(|l| serde_json::from_str(l).unwrap())
        .collect()
}

#[test]
fn success_sends_once_and_outbox_stays_empty() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    let t = FakeTransport::new(vec![], Ok(()));
    deliver(&t, &cfg(Some("http://x")), &o, &json!({"a":1}), NOW);
    assert_eq!(t.calls(), vec![json!({"a":1})]);
    assert_eq!(o.len(), 0);
    assert!(!o.path.exists());
}

#[test]
fn failure_queues_then_next_call_flushes_in_order() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    let c = cfg(Some("http://x"));
    let t1 = FakeTransport::new(vec![], Err(SendError::Network));
    deliver(&t1, &c, &o, &json!({"n":1}), NOW);
    assert_eq!(o.len(), 1);
    assert_eq!(read_lines(&o)[0], json!({"t":NOW,"body":{"n":1}}));
    let t2 = FakeTransport::new(vec![], Ok(()));
    deliver(&t2, &c, &o, &json!({"n":2}), NOW + 1);
    assert_eq!(t2.calls(), vec![json!({"n":1}), json!({"n":2})]);
    assert_eq!(o.len(), 0);
    assert!(!o.path.exists());
}

#[test]
fn retry_stops_at_first_failure_and_keeps_rest_queued() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    let c = cfg(Some("http://x"));
    let t1 = FakeTransport::new(vec![], Err(SendError::Network));
    deliver(&t1, &c, &o, &json!({"n":1}), NOW);
    deliver(&t1, &c, &o, &json!({"n":2}), NOW);
    let t2 = FakeTransport::new(vec![], Err(SendError::Status(503)));
    deliver(&t2, &c, &o, &json!({"n":3}), NOW);
    assert_eq!(t2.calls(), vec![json!({"n":1})]);
    let bodies: Vec<Value> = read_lines(&o).iter().map(|l| l["body"].clone()).collect();
    assert_eq!(bodies, vec![json!({"n":1}), json!({"n":2}), json!({"n":3})]);
}

#[test]
fn outbox_caps_at_1000_and_7_days() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    fs::create_dir_all(o.path.parent().unwrap()).unwrap();
    let mut s = String::new();
    for i in 0..1005u64 {
        let t = if i < 3 {
            NOW - 8 * DAY + i
        } else {
            NOW - 100 + i
        };
        s.push_str(&format!("{}\n", json!({"t":t,"body":{"i":i}})));
    }
    fs::write(&o.path, s).unwrap();
    let t = FakeTransport::new(vec![], Err(SendError::Network));
    deliver(&t, &cfg(Some("http://x")), &o, &json!({"i":9999}), NOW);
    assert_eq!(o.len(), 1000);
    let lines = read_lines(&o);
    assert!(lines
        .iter()
        .all(|l| l["t"].as_u64().unwrap() >= NOW - 7 * DAY));
    // 1002 fresh + 1 new = 1003; newest 1000 kept, so i = 3,4,5 dropped.
    assert_eq!(lines[0]["body"]["i"], 6);
    assert_eq!(lines[999]["body"]["i"], 9999);
}

#[test]
fn permanent_4xx_dropped() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    let c = cfg(Some("http://x"));
    let t = FakeTransport::new(vec![], Err(SendError::Status(400)));
    deliver(&t, &c, &o, &json!({"n":1}), NOW);
    assert_eq!(o.len(), 0);
    let t = FakeTransport::new(vec![], Err(SendError::Status(429)));
    deliver(&t, &c, &o, &json!({"n":2}), NOW);
    assert_eq!(o.len(), 1);
    let t = FakeTransport::new(vec![], Err(SendError::Status(408)));
    deliver(&t, &c, &o, &json!({"n":3}), NOW);
    assert_eq!(o.len(), 2);
}

#[test]
fn queued_permanent_rejection_is_dropped_and_loop_continues() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    let c = cfg(Some("http://x"));
    let t = FakeTransport::new(vec![], Err(SendError::Network));
    deliver(&t, &c, &o, &json!({"n":1}), NOW);
    let t = FakeTransport::new(vec![Err(SendError::Status(400)), Ok(())], Ok(()));
    deliver(&t, &c, &o, &json!({"n":2}), NOW);
    assert_eq!(t.calls().len(), 2);
    assert_eq!(o.len(), 0);
}

#[test]
fn no_endpoint_no_calls() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    let t = FakeTransport::new(vec![], Ok(()));
    deliver(&t, &cfg(None), &o, &json!({"a":1}), NOW);
    assert!(t.calls().is_empty());
    assert!(!o.path.exists());
}

#[test]
fn len_skips_unparsable_lines_and_missing_file_is_zero() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    assert_eq!(o.len(), 0);
    fs::create_dir_all(o.path.parent().unwrap()).unwrap();
    fs::write(&o.path, "garbage\n{\"t\":1,\"body\":{}}\n").unwrap();
    assert_eq!(o.len(), 1);
}

#[test]
fn unreachable_endpoint_returns_quickly() {
    if std::env::var_os("CI_NO_NET").is_some() {
        return;
    }
    let start = Instant::now();
    let r = UreqTransport.post("http://10.255.255.1:9/", &[], b"{}");
    assert!(r.is_err());
    assert!(start.elapsed() < Duration::from_secs(3));
}
