use std::collections::HashMap;
use std::fs::{self, File};
use std::path::Path;
use std::time::{Duration, SystemTime};
use wake::config::Env;
use wake::state::*;

struct MapEnv(HashMap<String, String>);
impl Env for MapEnv {
    fn var(&self, key: &str) -> Option<String> {
        self.0.get(key).cloned()
    }
}

fn age(path: &Path, secs: u64) {
    let f = File::options().write(true).open(path).unwrap();
    f.set_modified(SystemTime::now() - Duration::from_secs(secs))
        .unwrap();
}

fn open_state() -> SessionState {
    SessionState {
        offset: 7,
        open: vec![OpenInvocation {
            span_key: "k".into(),
            name: "s".into(),
            ..Default::default()
        }],
        ..Default::default()
    }
}

const MIN: u64 = 60;
const DAY: u64 = 86_400;

#[test]
fn state_dir_rules() {
    let home = Path::new("/h");
    let e = MapEnv(HashMap::from([("XDG_STATE_HOME".into(), "/x".into())]));
    assert_eq!(state_dir(&e, home), Path::new("/x/wake"));
    let e = MapEnv(HashMap::from([("XDG_STATE_HOME".into(), "".into())]));
    assert_eq!(state_dir(&e, home), Path::new("/h/.local/state/wake"));
    let e = MapEnv(HashMap::new());
    assert_eq!(state_dir(&e, home), Path::new("/h/.local/state/wake"));
}

#[test]
fn lock_is_exclusive() {
    let d = tempfile::tempdir().unwrap();
    let g = lock_session(d.path(), "s1").expect("first lock");
    assert!(lock_session(d.path(), "s1").is_none());
    drop(g);
    assert!(lock_session(d.path(), "s1").is_some());
}

#[test]
fn save_load_roundtrip() {
    let d = tempfile::tempdir().unwrap();
    assert!(load(d.path(), "s1").is_none());
    save(d.path(), "s1", &open_state()).unwrap();
    let s = load(d.path(), "s1").unwrap();
    assert_eq!(s.offset, 7);
    assert_eq!(s.open.len(), 1);
    fs::write(d.path().join("sessions/bad.json"), "{nope").unwrap();
    assert!(load(d.path(), "bad").is_none());
}

#[test]
fn init_offset_rules() {
    assert_eq!(init_offset(Some("startup"), 99), 0);
    for s in ["resume", "fork", "clear", "compact"] {
        assert_eq!(init_offset(Some(s), 99), 99);
    }
    assert_eq!(init_offset(None, 99), 99);
}

#[test]
fn gc_removes_old_only() {
    let d = tempfile::tempdir().unwrap();
    save(d.path(), "old", &SessionState::default()).unwrap();
    save(d.path(), "new", &SessionState::default()).unwrap();
    drop(lock_session(d.path(), "old"));
    drop(lock_session(d.path(), "new"));
    let s = d.path().join("sessions");
    age(&s.join("old.json"), 15 * DAY);
    age(&s.join("old.lock"), 15 * DAY);
    age(&s.join("new.json"), DAY);
    age(&s.join("new.lock"), DAY);
    gc(d.path(), SystemTime::now());
    assert!(!s.join("old.json").exists());
    assert!(!s.join("old.lock").exists());
    assert!(s.join("new.json").exists());
    assert!(s.join("new.lock").exists());
}

#[test]
fn gc_keeps_open_and_held() {
    let d = tempfile::tempdir().unwrap();
    save(d.path(), "open", &open_state()).unwrap();
    save(d.path(), "held", &SessionState::default()).unwrap();
    let g = lock_session(d.path(), "held").unwrap();
    let s = d.path().join("sessions");
    for f in ["open.json", "held.json", "held.lock"] {
        age(&s.join(f), 15 * DAY);
    }
    gc(d.path(), SystemTime::now());
    assert!(s.join("open.json").exists());
    assert!(s.join("held.lock").exists());
    drop(g);
}

#[test]
fn stale_open_finds_idle_sessions_only() {
    let d = tempfile::tempdir().unwrap();
    let s = d.path().join("sessions");
    save(d.path(), "idle", &open_state()).unwrap();
    save(d.path(), "fresh", &open_state()).unwrap();
    save(d.path(), "empty", &SessionState::default()).unwrap();
    save(d.path(), "locked", &open_state()).unwrap();
    let pending = SessionState {
        pending_agents: vec![PendingAgent::default()],
        ..Default::default()
    };
    save(d.path(), "pend", &pending).unwrap();
    age(&s.join("idle.json"), 31 * MIN);
    age(&s.join("fresh.json"), 5 * MIN);
    age(&s.join("empty.json"), 31 * MIN);
    age(&s.join("locked.json"), 31 * MIN);
    age(&s.join("pend.json"), 31 * MIN);
    let _held = lock_session(d.path(), "locked").unwrap();
    let mut got = stale_open(d.path(), SystemTime::now());
    got.sort_by(|a, b| a.0.cmp(&b.0));
    let ids: Vec<&str> = got.iter().map(|(i, _, _)| i.as_str()).collect();
    assert_eq!(ids, ["idle", "pend"]);
    // the returned guards hold the locks
    assert!(lock_session(d.path(), "idle").is_none());
}

#[test]
fn bad_session_id_rejected() {
    let d = tempfile::tempdir().unwrap();
    assert!(lock_session(d.path(), "../x").is_none());
    assert!(load(d.path(), "../x").is_none());
    assert!(save(d.path(), "../x", &SessionState::default()).is_err());
    assert!(
        !d.path().join("sessions").exists()
            || fs::read_dir(d.path().join("sessions"))
                .unwrap()
                .next()
                .is_none()
    );
    assert!(!d.path().join("x.lock").exists());
    assert!(!d.path().join("x.json").exists());
}
