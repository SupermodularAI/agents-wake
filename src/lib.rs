pub mod check;
pub mod config;
pub mod event;
pub mod harness;
pub mod identity;
pub mod ids;
pub mod install;
pub mod otlp;
pub mod repo;
pub mod send;
pub mod state;

/// Run `f`, swallowing any panic without printing anything.
///
/// Installs a silent panic hook so nothing reaches stderr, then catches the unwind.
/// Relies on unwinding, so the release profile must not use `panic = "abort"`.
pub fn run_silently<F: FnOnce() + std::panic::UnwindSafe>(f: F) {
    std::panic::set_hook(Box::new(|_| {}));
    let _ = std::panic::catch_unwind(f);
}

#[cfg(test)]
mod tests {
    use super::run_silently;

    #[test]
    fn run_silently_swallows_a_panic() {
        run_silently(|| panic!("boom"));
    }

    #[test]
    fn run_silently_runs_the_closure() {
        let mut ran = false;
        run_silently(std::panic::AssertUnwindSafe(|| ran = true));
        assert!(ran);
    }
}
