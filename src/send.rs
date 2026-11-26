//! Delivery of OTLP payloads: a ureq transport plus a small, bounded on-disk outbox.
//!
//! Nothing here prints, and filesystem errors on the outbox are ignored (best effort).

use crate::config::Config;
use serde_json::Value;
use std::fs;
use std::io::Write;
use std::path::PathBuf;
use std::time::{Duration, Instant};

const MAX_AGE_SECS: u64 = 7 * 86_400;
const MAX_ENTRIES: usize = 1000;
/// Most queued payloads merged into one POST (R28).
const BATCH: usize = 50;
/// No POST starts once a flush has run this long (R28).
const FLUSH_DEADLINE: Duration = Duration::from_secs(5);

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SendError {
    Network,
    Status(u16),
}

impl SendError {
    /// A 4xx other than 408/429 will never succeed on retry.
    fn is_permanent(&self) -> bool {
        matches!(self, SendError::Status(c) if (400..500).contains(c) && *c != 408 && *c != 429)
    }
}

pub trait Transport {
    fn post(&self, url: &str, headers: &[(String, String)], body: &[u8]) -> Result<(), SendError>;
}

/// HTTP transport: 2 s connect timeout, 2 s overall; any 2xx is success.
pub struct UreqTransport;

fn has_control(s: &str) -> bool {
    s.bytes().any(|b| b < 0x20 || b == 0x7f)
}

impl Transport for UreqTransport {
    fn post(&self, url: &str, headers: &[(String, String)], body: &[u8]) -> Result<(), SendError> {
        let agent = ureq::AgentBuilder::new()
            .timeout_connect(Duration::from_secs(2))
            .timeout(Duration::from_secs(2))
            .build();
        let mut req = agent.post(url).set("Content-Type", "application/json");
        for (k, v) in headers {
            if k.is_empty() || has_control(k) || has_control(v) {
                continue;
            }
            req = req.set(k, v);
        }
        match req.send_bytes(body) {
            Ok(_) => Ok(()),
            Err(ureq::Error::Status(code, _)) => Err(SendError::Status(code)),
            Err(_) => Err(SendError::Network),
        }
    }
}

pub struct Outbox {
    pub path: PathBuf,
}

struct Entry {
    t: u64,
    body: Value,
}

impl Outbox {
    /// Number of parsable entries; 0 if the file is missing.
    pub fn len(&self) -> usize {
        self.read().len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    fn read(&self) -> Vec<Entry> {
        let Ok(text) = fs::read_to_string(&self.path) else {
            return vec![];
        };
        text.lines()
            .filter_map(|l| {
                let v: Value = serde_json::from_str(l).ok()?;
                let t = v.get("t")?.as_u64()?;
                let body = v.get("body")?.clone();
                Some(Entry { t, body })
            })
            .collect()
    }

    /// Prune, then rewrite atomically (or remove the file when nothing is left).
    fn write(&self, mut entries: Vec<Entry>, now_secs: u64) {
        prune(&mut entries, now_secs);
        self.store(&entries);
    }

    /// Rewrite atomically, or remove the file when nothing is left.
    fn store(&self, entries: &[Entry]) {
        if entries.is_empty() {
            let _ = fs::remove_file(&self.path);
            return;
        }
        let _ = self.write_atomic(entries);
    }

    fn write_atomic(&self, entries: &[Entry]) -> std::io::Result<()> {
        use std::os::unix::fs::OpenOptionsExt;
        if let Some(dir) = self.path.parent() {
            crate::state::create_private_dir(dir)?;
        }
        let mut tmp = self.path.clone().into_os_string();
        tmp.push(".tmp");
        let tmp = PathBuf::from(tmp);
        let mut out = Vec::new();
        for e in entries {
            let line = serde_json::json!({"t": e.t, "body": e.body});
            serde_json::to_writer(&mut out, &line)?;
            out.push(b'\n');
        }
        // A leftover tmp from a killed run may have looser permissions; start from a new file.
        let _ = fs::remove_file(&tmp);
        let res = fs::File::options()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(&tmp)
            .and_then(|mut f| f.write_all(&out))
            .and_then(|_| fs::rename(&tmp, &self.path));
        if res.is_err() {
            let _ = fs::remove_file(&tmp);
        }
        res
    }
}

/// Drop entries older than 7 days, then all but the newest 1,000.
fn prune(entries: &mut Vec<Entry>, now_secs: u64) {
    let cutoff = now_secs.saturating_sub(MAX_AGE_SECS);
    entries.retain(|e| e.t >= cutoff);
    if entries.len() > MAX_ENTRIES {
        entries.drain(..entries.len() - MAX_ENTRIES);
    }
}

/// The `resourceSpans` array of an ExportTraceServiceRequest body, if it is one.
fn resource_spans(body: &Value) -> Option<&Vec<Value>> {
    body.get("resourceSpans")?.as_array()
}

/// How many leading entries go in the next POST: up to [`BATCH`] OTLP payloads, or a single
/// entry that is not one.
fn batch_len(entries: &[Entry]) -> usize {
    entries
        .iter()
        .take(BATCH)
        .take_while(|e| resource_spans(&e.body).is_some())
        .count()
        .max(1)
        .min(entries.len())
}

/// One entry is sent as queued; several are merged into one ExportTraceServiceRequest.
fn batch_body(batch: &[Entry]) -> serde_json::Result<Vec<u8>> {
    if let [one] = batch {
        return serde_json::to_vec(&one.body);
    }
    let spans: Vec<&Value> = batch
        .iter()
        .filter_map(|e| resource_spans(&e.body))
        .flatten()
        .collect();
    serde_json::to_vec(&serde_json::json!({ "resourceSpans": spans }))
}

/// Append `payload` to the outbox (pruned and rewritten atomically). Errors are ignored.
pub fn enqueue(outbox: &Outbox, payload: &Value, now_secs: u64) {
    let mut entries = outbox.read();
    entries.push(Entry {
        t: now_secs,
        body: payload.clone(),
    });
    outbox.write(entries, now_secs);
}

/// Send queued entries oldest-first, up to [`BATCH`] per POST (R28). No POST starts after
/// [`FLUSH_DEADLINE`]. A batch that succeeds or is rejected for good (4xx other than 408/429) is
/// removed and the outbox rewritten before the next POST, so a kill mid-flush resends at most one
/// batch. The first transient failure ends the flush. An empty outbox makes no call.
pub fn flush(t: &dyn Transport, cfg: &Config, outbox: &Outbox, now_secs: u64) {
    let Some(url) = cfg.endpoint.as_deref() else {
        return;
    };
    let mut queued = outbox.read();
    if queued.is_empty() {
        return;
    }
    let before = queued.len();
    prune(&mut queued, now_secs);
    let mut dirty = queued.len() != before;
    let start = Instant::now();
    while !queued.is_empty() && start.elapsed() < FLUSH_DEADLINE {
        let n = batch_len(&queued);
        if let Ok(body) = batch_body(&queued[..n]) {
            match t.post(url, &cfg.headers, &body) {
                Ok(()) => {}
                Err(e) if e.is_permanent() => {}
                Err(_) => break,
            }
        }
        queued.drain(..n);
        outbox.store(&queued);
        dirty = false;
    }
    if dirty {
        outbox.store(&queued);
    }
}

/// Queue `payload` behind anything already queued, then flush. Failures stay queued.
pub fn deliver(t: &dyn Transport, cfg: &Config, outbox: &Outbox, payload: &Value, now_secs: u64) {
    if cfg.endpoint.is_none() {
        return;
    }
    enqueue(outbox, payload, now_secs);
    flush(t, cfg, outbox, now_secs);
}
