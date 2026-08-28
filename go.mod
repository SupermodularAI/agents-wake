module github.com/SupermodularAI/agents-wake

// The floor we promise, not the toolchain we happen to develop with: one release
// behind current, matching Go's own two-release support window. Deliberately
// major-minor — a patch floor would force a toolchain download on anyone
// slightly behind, and `go install` is a supported install path (plan §7).
// CI verifies this floor rather than only asserting it (.github/workflows/ci.yml).
go 1.25

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/spf13/cobra v1.10.2
	golang.org/x/term v0.40.0
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	golang.org/x/sys v0.41.0 // indirect
)
