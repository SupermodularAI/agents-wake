// Package version carries the build identity of the binary.
//
// Every value here is injected at link time via -ldflags: by the Makefile for a
// local build, and by .goreleaser.yaml for an official release artefact. The
// defaults are deliberately not real version numbers: a binary reporting "dev"
// is one built outside either pipeline, and that should be visible rather than
// indistinguishable from a release.
package version

// Untagged is the Version of a binary built outside the Makefile and
// .goreleaser.yaml pipelines — neither pipeline can produce it, so a binary
// reporting it has no release tag to compare itself against.
const Untagged = "dev"

var (
	// Version is the release version, from `git describe`.
	Version = Untagged
	// Commit is the short commit hash the binary was built from.
	Commit = "none"
	// Date is the RFC3339 UTC build timestamp.
	Date = "unknown"
)

// String renders the three values as a single line for `wake --version`.
func String() string {
	return Version + " (commit " + Commit + ", built " + Date + ")"
}
