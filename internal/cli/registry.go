package cli

import "github.com/spf13/cobra"

// commands holds a constructor per subcommand. The root command attaches all of
// them at build time.
//
// Every subcommand lives in its own file and appends itself here from an init(),
// so adding one touches no existing file:
//
//	// internal/cli/cost.go
//	func init() { commands = append(commands, newCostCmd) }
//
//	func newCostCmd() *cobra.Command { ... }
//
// This exists for a mechanical reason rather than an aesthetic one. Ten tickets
// in plan §7.3 each add a subcommand — cost, scan, unused, doctor, init,
// uninstall, ingest, watch, stream, report. If registration happened in a shared
// list or inside newRootCmd, every pair of those worked on in parallel would
// conflict on the same line, ten times over, for no design reason. Appending
// from an init() in a new file means parallel lanes never touch shared lines.
//
// Constructors, not values: a *cobra.Command carries parsed flag state, so a
// package-level instance would leak state between tests and between a real run
// and its tests.
var commands []func() *cobra.Command
