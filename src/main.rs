use std::io::Read;
use std::path::PathBuf;
use std::process::ExitCode;
use std::time::SystemTime;
use wake::config::ProcessEnv;
use wake::harness::claude_code::{run_hook, Deps};
use wake::send::UreqTransport;

const USAGE: &str = "usage: wake <emit <hook> | check | install | uninstall | --version>";
const INSTALL_USAGE: &str = "usage: wake install [--harness claude-code] [--endpoint URL] [--header K=V]... [--identity email|anonymous]\n       wake uninstall [--harness claude-code] [--purge]";

/// `wake emit <hook>`: read the payload, then handle it. Never prints, never fails.
fn emit(hook: &str) {
    let mut stdin = Vec::new();
    let _ = std::io::stdin().read_to_end(&mut stdin);
    let Some(home) = std::env::var_os("HOME").filter(|h| !h.is_empty()) else {
        return;
    };
    let Some(exe_dir) = std::env::current_exe()
        .ok()
        .and_then(|p| p.parent().map(PathBuf::from))
    else {
        return;
    };
    let deps = Deps {
        env: &ProcessEnv,
        home: PathBuf::from(home),
        exe_dir,
        transport: &UreqTransport,
        now: SystemTime::now(),
    };
    run_hook(hook, &stdin, &deps);
}

/// `wake check`: print the resolved config and send a test event; returns the exit code.
fn check() -> u8 {
    let (Some(home), Ok(exe)) = (
        std::env::var_os("HOME").filter(|h| !h.is_empty()),
        std::env::current_exe(),
    ) else {
        eprintln!("wake check: cannot resolve HOME or the executable path");
        return 1;
    };
    let deps = Deps {
        env: &ProcessEnv,
        home: PathBuf::from(home),
        exe_dir: exe.parent().map(PathBuf::from).unwrap_or_default(),
        transport: &UreqTransport,
        now: SystemTime::now(),
    };
    let code = wake::check::run(&deps, &mut std::io::stdout());
    u8::try_from(code).unwrap_or(1)
}

/// The flags of `install` / `uninstall`, parsed by hand.
struct Flags {
    install: wake::install::InstallArgs,
    purge: bool,
}

fn parse_flags(args: &[String], uninstall: bool) -> Result<Flags, String> {
    let mut f = Flags {
        install: wake::install::InstallArgs {
            endpoint: None,
            headers: vec![],
            identity: None,
            harness: "claude-code".into(),
        },
        purge: false,
    };
    let mut it = args.iter();
    while let Some(flag) = it.next() {
        let mut value = || {
            it.next()
                .cloned()
                .ok_or_else(|| format!("{flag} needs a value"))
        };
        match (flag.as_str(), uninstall) {
            ("--harness", _) => f.install.harness = value()?,
            ("--purge", true) => f.purge = true,
            ("--endpoint", false) => f.install.endpoint = Some(value()?),
            ("--header", false) => {
                let v = value()?;
                match v.split_once('=') {
                    Some((k, val)) if !k.trim().is_empty() => f
                        .install
                        .headers
                        .push((k.trim().to_string(), val.trim().to_string())),
                    _ => return Err("--header must be K=V".into()),
                }
            }
            ("--identity", false) => {
                let v = value()?;
                if v != "email" && v != "anonymous" {
                    return Err("--identity must be email or anonymous".into());
                }
                f.install.identity = Some(v);
            }
            _ => return Err(format!("unknown argument: {flag}")),
        }
    }
    if f.install.harness != "claude-code" {
        return Err(format!(
            "unsupported harness: {} (only claude-code)",
            f.install.harness
        ));
    }
    Ok(f)
}

/// `wake install` / `wake uninstall`: summary to stdout and 0, or a message to stderr and 2.
fn install_cmd(args: &[String], uninstall: bool) -> u8 {
    let name = if uninstall { "uninstall" } else { "install" };
    let flags = match parse_flags(args, uninstall) {
        Ok(f) => f,
        Err(e) => {
            eprintln!("wake {name}: {e}\n{INSTALL_USAGE}");
            return 2;
        }
    };
    let Some(home) = std::env::var_os("HOME").filter(|h| !h.is_empty()) else {
        eprintln!("wake {name}: HOME is not set");
        return 2;
    };
    let exe = match std::env::current_exe().and_then(|p| p.canonicalize()) {
        Ok(p) => p,
        Err(e) => {
            eprintln!("wake {name}: cannot resolve the executable path: {e}");
            return 2;
        }
    };
    let home = PathBuf::from(home);
    let result = if uninstall {
        wake::install::uninstall(&ProcessEnv, &home, &exe, flags.purge)
    } else {
        wake::install::install(
            &ProcessEnv,
            &home,
            &exe,
            &flags.install,
            &mut wake::install::TerminalPrompter,
        )
    };
    match result {
        Ok(summary) => {
            print!("{summary}");
            0
        }
        Err(e) => {
            eprintln!("wake {name}: {e}");
            2
        }
    }
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().collect();
    match args.get(1).map(String::as_str) {
        Some("emit") => {
            // emit must never fail, print, or panic outward.
            let hook = args.get(2).map(String::as_str).unwrap_or("");
            wake::run_silently(|| emit(hook));
            ExitCode::SUCCESS
        }
        Some("check") => ExitCode::from(check()),
        Some("install") => ExitCode::from(install_cmd(&args[2..], false)),
        Some("uninstall") => ExitCode::from(install_cmd(&args[2..], true)),
        Some("--version" | "-V") => {
            println!("wake {}", env!("CARGO_PKG_VERSION"));
            ExitCode::SUCCESS
        }
        _ => {
            eprintln!("{USAGE}");
            ExitCode::from(2)
        }
    }
}
