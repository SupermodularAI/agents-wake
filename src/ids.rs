//! Deterministic trace and span ids, so retries are idempotent.

use sha2::{Digest, Sha256};

fn digest(prefix: &str, key: &str) -> [u8; 32] {
    let mut h = Sha256::new();
    h.update(prefix.as_bytes());
    h.update([0u8]);
    h.update(key.as_bytes());
    h.finalize().into()
}

/// `sha256("wake-trace\0" + session_id)[..16]`
pub fn trace_id(session_id: &str) -> [u8; 16] {
    let mut out = [0u8; 16];
    out.copy_from_slice(&digest("wake-trace", session_id)[..16]);
    out
}

/// `sha256("wake-span\0" + key)[..8]`
pub fn span_id(key: &str) -> [u8; 8] {
    let mut out = [0u8; 8];
    out.copy_from_slice(&digest("wake-span", key)[..8]);
    out
}
