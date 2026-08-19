package config

import (
	"fmt"
	"os"
)

// RemoveConfigRoot deletes the config root and everything in it: config.toml, the
// repository-id salt, and the three lock files.
//
// It lives here for the reason ADR-0019 gives for every other read and write of the
// salt — all access to it stays inside this package, which is what makes "it never
// travels" checkable by reading one directory. Deletion is no exception: an
// os.RemoveAll of ConfigDir spelled in another package would be the first call site
// an auditor of that guarantee has to go and find.
//
// This is the destructive, stated operation ADR-0019 §3 contrasts with an implicit
// rotation: it is only ever reached from `wake uninstall`, which discloses the path
// first. `wake remove --purge` deliberately does not call it (ADR-0014: the config
// root is what survives a deleted data root).
//
// A config root that is not there is success rather than a fault — `uninstall` on a
// machine that was never `init`ed has nothing here to remove — which is os.RemoveAll's
// own behaviour and the reason it is used directly.
//
// The error names the config root and nothing inside it: the salt's bytes are the
// secret, and this package's rule is that no error it returns carries them.
func RemoveConfigRoot(p Paths) error {
	if err := os.RemoveAll(p.ConfigDir); err != nil {
		return fmt.Errorf("removing %s: %w", p.ConfigDir, err)
	}
	return nil
}
