use std::io::Read;
use std::process::ExitCode;

const USAGE: &str = "usage: wake <emit <hook> | check | install | uninstall>";

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().collect();
    match args.get(1).map(String::as_str) {
        Some("emit") => {
            // emit must never fail, print, or panic outward.
            wake::run_silently(|| {
                let mut stdin = Vec::new();
                let _ = std::io::stdin().read_to_end(&mut stdin);
                let hook = args.get(2).map(String::as_str).unwrap_or("");
                wake::harness::claude_code::run_hook(hook, &stdin);
            });
            ExitCode::SUCCESS
        }
        // Stubs until the check and install tasks land.
        Some("check") | Some("install") | Some("uninstall") => ExitCode::SUCCESS,
        _ => {
            eprintln!("{USAGE}");
            ExitCode::from(2)
        }
    }
}
