//go:build !unix

package config

import "io/fs"

// ownedByCaller reports that this platform has no file ownership this package
// knows how to inspect. The released targets are darwin and linux (as
// internal/lockfile's own fallback records), so nothing here is reached on a
// supported build.
//
// It answers "unknown" rather than "not owned". Unlike a lock that cannot be
// taken, an ownership question that cannot be asked is not a reason to refuse: it
// would refuse every installation on the platform, over a property the check never
// established either way.
func ownedByCaller(fs.FileInfo) (owned, known bool) { return false, false }
