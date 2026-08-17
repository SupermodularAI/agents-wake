// Package detach starts a child process that outlives the process that started it.
//
// It exists for one caller: the Claude Code trigger. ADR-0016 requires the child to
// be "genuinely detached (own process group) so it survives the terminal closing" —
// a session-end hook whose scan is still running when the terminal goes away must
// finish, or the events of the session that just ended are the ones that never get
// collected.
//
// Own process group, not merely a background goroutine: a shell or a harness that
// signals the foreground process group on terminal close would take a same-group
// child with it, and the parent here exits immediately by design.
//
// Platform coverage follows internal/lockfile's shape: the released targets are
// darwin and linux, and anything else fails loudly rather than silently running an
// attached child. T109 does not settle Windows support; plan §3.2 owns it.
package detach

import (
	"errors"
	"fmt"
	"os/exec"
)

// errNoCommand rejects an empty argv. Lowercase and unpunctuated for revive's
// error-strings rule, like every other error in this module.
var errNoCommand = errors.New("no command to start")

// Start runs argv[0] with argv[1:] in a new session and process group, with all
// three standard streams connected to the null device, and returns without waiting
// for it.
//
// stdio goes to the null device rather than being inherited: the trigger must not
// write to a session's terminal, and an inherited descriptor is how output escapes
// a process that is otherwise silent (ADR-0016). os/exec documents that a nil
// stream is connected to the null device, so leaving all three unset is the
// statement of that, not an omission.
//
// Deliberately no Wait. The parent exits and the child is reparented, so there is
// no zombie to reap — waiting is exactly what would make the hook slow, which is
// the thing that gets a trigger turned off.
func Start(argv []string) error {
	if err := supported(); err != nil {
		return err
	}
	if len(argv) == 0 {
		return errNoCommand
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		// The path is named because this error never reaches a user: the trigger
		// discards it, and the caller that does not is one running interactively.
		return fmt.Errorf("starting %s: %w", argv[0], err)
	}
	return nil
}
