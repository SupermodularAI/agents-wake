module github.com/SupermodularAI/agents-wake

// The floor we promise, not the toolchain we happen to develop with: one release
// behind current, matching Go's own two-release support window. Deliberately
// major-minor — a patch floor would force a toolchain download on anyone
// slightly behind, and `go install` is a supported install path (plan §7).
// CI verifies this floor rather than only asserting it (.github/workflows/ci.yml).
//
// modernc.org/sqlite is the fourth direct dependency and the first one that is not
// a CLI or encoding library. It is here because a harness keeps its history in a
// SQLite store and there is no other way to read one; it is *this* driver because
// it is pure Go, and the release matrix cross-compiles darwin and linux on amd64
// and arm64 with CGO_ENABLED=0, which rules out every cgo driver before any other
// consideration. It is reachable only through internal/sqlitex, whose driver
// import lives in a file of its own so a test can assert the confinement over the
// whole module rather than trusting a convention (ADR-0009).
//
// It is pinned below the latest release deliberately: releases from v1.39.0 on
// declare a patch-level go directive (go 1.25.0), which would raise the floor
// above and force the very toolchain download the paragraph above refuses.
go 1.25

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/spf13/cobra v1.10.2
	golang.org/x/term v0.40.0
	modernc.org/sqlite v1.38.2
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	golang.org/x/exp v0.0.0-20250620022241-b7579e27df2b // indirect
	golang.org/x/sys v0.41.0 // indirect
	modernc.org/libc v1.66.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
