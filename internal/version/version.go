// Package version carries the build identity of the binary.
//
// Every value here is injected at link time via -ldflags (see Taskfile.yml).
// The defaults are deliberately not real version numbers: a binary reporting
// "dev" is one built outside the task pipeline, and that should be visible
// rather than indistinguishable from a release.
package version

var (
	// Version is the release version, from `git describe`.
	Version = "dev"
	// Commit is the short commit hash the binary was built from.
	Commit = "none"
	// Date is the RFC3339 UTC build timestamp.
	Date = "unknown"
)

// String renders the three values as a single line for `wake --version`.
func String() string {
	return Version + " (commit " + Commit + ", built " + Date + ")"
}
