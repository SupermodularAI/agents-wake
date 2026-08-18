// Package keyeddigest computes one thing: HMAC-SHA256 of a message under a
// caller-supplied key.
//
// It exists because four sites derived the same construction independently — the
// repository id, the project-table match digest and the derived-name subkey (all
// in internal/config), plus the persisted scope digest (internal/record) — so a
// correction to it had to be applied and re-verified in four places, and nothing
// caught a site that drifted (ADR-0022).
//
// It is deliberately domain-blind: it has no concept of a repository id, a scope
// or a salt, it reads and writes nothing, and it holds no state across calls.
// That is the whole basis on which ADR-0022 narrows ADR-0019 ("all access ...
// stays inside internal/config") and ADR-0020 ("record ... imports nothing from
// the repo") to admit it — a function that computes a digest and forgets its
// input is not a second place that can hold key material. A package variable, a
// pooled MAC state or a log line here would remove that justification.
//
// It does not hex-encode and it does not truncate. Its callers disagree about
// both: one uses the raw bytes as a key, one hex-encodes without truncating, and
// two truncate to different widths. Encoding is the caller's decision.
package keyeddigest

import (
	"crypto/hmac"
	"crypto/sha256"
)

// Sum returns the HMAC-SHA256 of data under key, as raw digest bytes.
//
// hash.Hash's contract is that Write never returns an error. It is checked
// anyway — errcheck runs with check-blank, and there is no honest way to discard
// it — and a violation is the one outcome worth stopping the process for: it
// would return a digest over a prefix of data, so every id, table entry and
// persisted name written afterwards would be keyed to a value nothing else
// agrees with. The panic names the operation and nothing else: neither the key
// nor the data may reach a message (plan §3.4, §4.2).
func Sum(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	if _, err := mac.Write(data); err != nil {
		panic("computing a keyed digest: " + err.Error())
	}
	return mac.Sum(nil)
}
