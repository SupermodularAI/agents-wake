package keyeddigest

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// RFC 4231 §4.3 (test case 2) pins the construction against a published vector
// rather than against the stdlib call this function wraps: every persisted
// digest in this tool is derived through here, so the one assertion worth
// having is one that cannot drift with the implementation.
func TestSumMatchesThePublishedHMACSHA256Vector(t *testing.T) {
	const want = "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"
	got := hex.EncodeToString(Sum([]byte("Jefe"), []byte("what do ya want for nothing?")))
	if got != want {
		t.Errorf("Sum() = %s, want %s", got, want)
	}
}

// The return is the full digest, unencoded and untruncated: config.Repos.NameKey
// uses these bytes as an HMAC key, and its two hex-encoding callers truncate to
// two different widths (ADR-0019 §8, ADR-0020, ADR-0022).
func TestSumReturnsAFullUntruncatedDigest(t *testing.T) {
	if got := len(Sum([]byte("key"), []byte("data"))); got != sha256.Size {
		t.Errorf("Sum() returned %d bytes, want %d", got, sha256.Size)
	}
}

// Both arguments matter, and nothing else does: no state survives a call, so
// interleaved calls cannot move a shared MAC forward (ADR-0022).
func TestSumDependsOnBothArgumentsAndHoldsNoState(t *testing.T) {
	base := hex.EncodeToString(Sum([]byte("key"), []byte("data")))
	if other := hex.EncodeToString(Sum([]byte("kez"), []byte("data"))); other == base {
		t.Error("Sum() ignored the key")
	}
	if other := hex.EncodeToString(Sum([]byte("key"), []byte("datb"))); other == base {
		t.Error("Sum() ignored the data")
	}
	if again := hex.EncodeToString(Sum([]byte("key"), []byte("data"))); again != base {
		t.Errorf("Sum() is not stateless: %s then %s", base, again)
	}
}

// An empty key is not this package's decision. record.Namer refuses a keyless
// scope digest ahead of the call (ADR-0020, fail closed, plan §3.4); a primitive
// that second-guessed that would put one fail-closed rule in two places.
func TestSumTreatsAnEmptyKeyAndEmptyDataAsOrdinaryInput(t *testing.T) {
	if got := len(Sum(nil, nil)); got != sha256.Size {
		t.Errorf("Sum(nil, nil) returned %d bytes, want %d", got, sha256.Size)
	}
}
