package sqlitex

// The only import of a SQLite driver in this module, kept to a file of its own so
// the confinement is a fact a test can assert rather than a convention (ADR-0009:
// internal/sqlitex is "the only way this codebase touches SQLite"). The driver is
// pure Go, which is what makes it usable at all here: the release matrix
// cross-compiles four targets with CGO_ENABLED=0, and every cgo driver is ruled
// out by that alone.
import _ "modernc.org/sqlite"

// driverName is the name modernc.org/sqlite registers itself under.
const driverName = "sqlite"
