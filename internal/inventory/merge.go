package inventory

import (
	"cmp"
	"slices"
)

// Merge folds further discoveries into a base one.
//
// It is in this package because Discovery carries an unexported fold map that no
// caller can construct, and because a merge is where two harnesses' primitives
// could silently collide. They cannot: every key below carries the harness, so a
// server both harnesses configure under one name stays two rows.
//
// ProjectScanned is taken from the base and never widened. It is a Claude Code
// notion — whether that harness's project-local discovery ran — and a second
// harness must not be able to flip it, because the snapshot writer reads it to
// decide whether a missing row is out of scope or gone.
func Merge(base Discovery, extra ...Discovery) Discovery {
	merged := Discovery{
		Primitives:     slices.Clone(base.Primitives),
		ProjectScanned: base.ProjectScanned,
		Observed:       slices.Clone(base.Observed),
		canonical:      map[identity]identity{},
	}
	for from, to := range base.canonical {
		merged.canonical[from] = to
	}
	held := map[identity]struct{}{}
	for _, primitive := range merged.Primitives {
		held[identityOf(primitive)] = struct{}{}
	}
	for _, discovery := range extra {
		for _, primitive := range discovery.Primitives {
			if _, seen := held[identityOf(primitive)]; seen {
				continue
			}
			held[identityOf(primitive)] = struct{}{}
			merged.Primitives = append(merged.Primitives, primitive)
		}
		merged.Observed = append(merged.Observed, discovery.Observed...)
		for from, to := range discovery.canonical {
			merged.canonical[from] = to
		}
	}
	// Sorted on all three parts, not on the kind and name sortedPrimitives orders
	// a single harness's rows by: with two harnesses in one slice, those two are
	// no longer a total order, and a renderer reading a different order on two
	// runs of one build is a diff nobody made.
	slices.SortFunc(merged.Primitives, func(left, right Primitive) int {
		return cmp.Or(
			cmp.Compare(left.Harness, right.Harness),
			cmp.Compare(left.Kind, right.Kind),
			cmp.Compare(left.Name, right.Name),
		)
	})
	return merged
}

// identityOf is one primitive's identity: harness, kind and name, which is the
// key everything in this package folds on.
func identityOf(primitive Primitive) identity {
	return identity{harness: primitive.Harness, kind: primitive.Kind, name: primitive.Name}
}
