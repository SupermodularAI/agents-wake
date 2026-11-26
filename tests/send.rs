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
/// An OTLP-shaped payload (one ExportTraceServiceRequest with one resourceSpans item).
fn p(n: u64) -> Value {
    json!({"resourceSpans":[{"n":n}]})
}
/// What a batch of `ns` payloads is merged into.
fn merged(ns: &[u64]) -> Value {
    json!({"resourceSpans": ns.iter().map(|n| json!({"n":n})).collect::<Vec<_>>()})
}
fn queue(o: &Outbox, ns: std::ops::Range<u64>) {
    for n in ns {
        enqueue(o, &p(n), NOW);
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
    deliver(&t, &cfg(Some("http://x")), &o, &p(1), NOW);
    assert_eq!(t.calls(), vec![p(1)]);
    assert_eq!(o.len(), 0);
    assert!(!o.path.exists());
}

#[test]
fn failure_queues_then_next_call_flushes_in_order() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    let c = cfg(Some("http://x"));
    let t1 = FakeTransport::new(vec![], Err(SendError::Network));
    deliver(&t1, &c, &o, &p(1), NOW);
    assert_eq!(o.len(), 1);
    assert_eq!(read_lines(&o)[0], json!({"t":NOW,"body":p(1)}));
    let t2 = FakeTransport::new(vec![], Ok(()));
    deliver(&t2, &c, &o, &p(2), NOW + 1);
    // R28: both queued payloads go in one POST, oldest first (was two POSTs before batching).
    assert_eq!(t2.calls(), vec![merged(&[1, 2])]);
    assert_eq!(o.len(), 0);
    assert!(!o.path.exists());
}

#[test]
fn retry_stops_at_first_failure_and_keeps_rest_queued() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    let c = cfg(Some("http://x"));
    let t1 = FakeTransport::new(vec![], Err(SendError::Network));
    queue(&o, 0..100);
    deliver(&t1, &c, &o, &p(100), NOW);
    let t2 = FakeTransport::new(vec![], Err(SendError::Status(503)));
    deliver(&t2, &c, &o, &p(101), NOW);
    // R28: the first batch (50 payloads) fails, so the other two batches are never posted.
    assert_eq!(t2.calls(), vec![merged(&(0..50).collect::<Vec<_>>())]);
    let bodies: Vec<Value> = read_lines(&o).iter().map(|l| l["body"].clone()).collect();
    assert_eq!(bodies, (0..102).map(p).collect::<Vec<_>>());
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
    deliver(&t, &c, &o, &p(1), NOW);
    assert_eq!(o.len(), 0);
    let t = FakeTransport::new(vec![], Err(SendError::Status(429)));
    deliver(&t, &c, &o, &p(2), NOW);
    assert_eq!(o.len(), 1);
    let t = FakeTransport::new(vec![], Err(SendError::Status(408)));
    deliver(&t, &c, &o, &p(3), NOW);
    assert_eq!(o.len(), 2);
}

#[test]
fn queued_permanent_rejection_is_dropped_and_loop_continues() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    let c = cfg(Some("http://x"));
    queue(&o, 0..50);
    // R28: 51 payloads are two batches; the first is rejected for good, the second still goes.
    let t = FakeTransport::new(vec![Err(SendError::Status(400)), Ok(())], Ok(()));
    deliver(&t, &c, &o, &p(50), NOW);
    assert_eq!(t.calls().len(), 2);
    assert_eq!(t.calls()[1], p(50));
    assert_eq!(o.len(), 0);
}

#[test]
fn no_endpoint_no_calls() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    let t = FakeTransport::new(vec![], Ok(()));
    deliver(&t, &cfg(None), &o, &p(1), NOW);
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

#[test]
fn batches_of_50_in_order() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    queue(&o, 0..120);
    let t = FakeTransport::new(vec![], Ok(()));
    flush(&t, &cfg(Some("http://x")), &o, NOW);
    let calls = t.calls();
    assert_eq!(calls.len(), 3);
    assert_eq!(calls[0], merged(&(0..50).collect::<Vec<_>>()));
    assert_eq!(calls[1], merged(&(50..100).collect::<Vec<_>>()));
    assert_eq!(calls[2], merged(&(100..120).collect::<Vec<_>>()));
    assert!(!o.path.exists());
}

#[test]
fn transient_failure_mid_flush_keeps_only_unsent() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    queue(&o, 0..120);
    let t = FakeTransport::new(vec![Ok(()), Err(SendError::Network)], Ok(()));
    flush(&t, &cfg(Some("http://x")), &o, NOW);
    assert_eq!(t.calls().len(), 2);
    let bodies: Vec<Value> = read_lines(&o).iter().map(|l| l["body"].clone()).collect();
    assert_eq!(bodies, (50..120).map(p).collect::<Vec<_>>());
}

#[test]
fn non_otlp_body_is_sent_alone() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    enqueue(&o, &p(1), NOW);
    enqueue(&o, &json!({"other":true}), NOW);
    enqueue(&o, &p(2), NOW);
    enqueue(&o, &p(3), NOW);
    let t = FakeTransport::new(vec![], Ok(()));
    flush(&t, &cfg(Some("http://x")), &o, NOW);
    assert_eq!(
        t.calls(),
        vec![p(1), json!({"other":true}), merged(&[2, 3])]
    );
}

/// Every POST takes 2 s (the per-request timeout); records the outbox size seen at each call.
struct SlowTransport {
    outbox: std::path::PathBuf,
    seen: RefCell<Vec<usize>>,
}
impl Transport for SlowTransport {
    fn post(&self, _url: &str, _h: &[(String, String)], _body: &[u8]) -> Result<(), SendError> {
        let o = Outbox {
            path: self.outbox.clone(),
        };
        self.seen.borrow_mut().push(o.len());
        std::thread::sleep(Duration::from_secs(2));
        Ok(())
    }
}

#[test]
fn flush_stops_at_deadline_and_keeps_progress() {
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    queue(&o, 0..200);
    let t = SlowTransport {
        outbox: o.path.clone(),
        seen: RefCell::new(vec![]),
    };
    let start = Instant::now();
    flush(&t, &cfg(Some("http://x")), &o, NOW);
    let took = start.elapsed();
    // POSTs start at 0, 2 and 4 s; at 6 s the 5 s deadline has passed, so the fourth never starts.
    assert!(
        took >= Duration::from_secs(5) && took < Duration::from_secs(7),
        "{took:?}"
    );
    // The outbox is rewritten after each successful batch, before the next POST.
    assert_eq!(*t.seen.borrow(), vec![200, 150, 100]);
    let bodies: Vec<Value> = read_lines(&o).iter().map(|l| l["body"].clone()).collect();
    assert_eq!(bodies, (150..200).map(p).collect::<Vec<_>>());
}

#[test]
fn outbox_is_private() {
    use std::os::unix::fs::PermissionsExt;
    let d = tempfile::tempdir().unwrap();
    let o = outbox(d.path());
    enqueue(&o, &p(1), NOW);
    let mode = |p: &Path| fs::metadata(p).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode(&o.path), 0o600);
    assert_eq!(mode(o.path.parent().unwrap()), 0o700);
}
