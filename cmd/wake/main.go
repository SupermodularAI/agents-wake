// Command wake is the entrypoint. It calls cli.Execute() and nothing else, so
// no logic can accumulate here where it would be untestable (plan §6, ADR-0001).
package main

import (
	"os"

	"github.com/SupermodularAI/agents-wake/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
