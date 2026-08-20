package selfupdate

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrCurlMissing reports that curl is not installed. Update is the only feature
// with a prerequisite beyond the binary itself (ADR-0026), so the refusal names
// it rather than surfacing an exec error.
var ErrCurlMissing = errors.New("curl is not installed: wake update needs curl on PATH to reach the network")

// Fetcher performs the two network steps update needs. Production is curl; a
// test substitutes a fake, which is why no in-process HTTP client exists here or
// in any test in this package (ADR-0026).
type Fetcher interface {
	// EffectiveURL follows url's redirects and returns the URL it lands on,
	// downloading no body.
	EffectiveURL(url string) (string, error)
	// Download writes url's body to dest, replacing whatever is there.
	Download(url, dest string) error
}

// CurlFetcher is the production Fetcher: one curl subprocess per call.
type CurlFetcher struct{ path string }

// NewCurlFetcher locates curl on PATH, returning ErrCurlMissing when it is absent.
func NewCurlFetcher() (CurlFetcher, error) {
	path, err := exec.LookPath("curl")
	if err != nil {
		return CurlFetcher{}, ErrCurlMissing
	}
	return CurlFetcher{path: path}, nil
}

// effectiveURLArgs is install.sh's `curl -fsSL -o /dev/null -w '%{url_effective}'`.
//
// The arguments are a pure function so a test can assert them without running
// curl: this is what pins the invocation to the installer's rather than letting
// the two drift.
func effectiveURLArgs(url string) []string {
	return []string{"-fsSL", "-o", os.DevNull, "-w", "%{url_effective}", url}
}

// downloadArgs is install.sh's `curl -fsSL -o "$dest" "$url"`.
func downloadArgs(url, dest string) []string { return []string{"-fsSL", "-o", dest, url} }

// EffectiveURL resolves url's redirects, printing the URL it lands on and
// discarding the body.
func (c CurlFetcher) EffectiveURL(url string) (string, error) {
	out, err := exec.Command(c.path, effectiveURLArgs(url)...).Output()
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", url, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Download writes url's body to dest.
//
// curl's own diagnostics are included in the failure, because a refusal that
// says only "exit status 22" leaves the user with nothing to act on. Nothing
// here reads a transcript, so the only strings that can appear are release-asset
// URLs and curl's messages.
func (c CurlFetcher) Download(url, dest string) error {
	out, err := exec.Command(c.path, downloadArgs(url, dest)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("downloading %s: %w: %s", url, err, bytes.TrimSpace(out))
	}
	return nil
}
