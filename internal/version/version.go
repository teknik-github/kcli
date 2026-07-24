// Package version reports the running kcli build and checks the module proxy
// for a newer release. It is dependency-free (net/http, encoding/json,
// runtime/debug) so it adds nothing to the build graph.
package version

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Module is the import path used both for the self-update command and the
// proxy query below.
const Module = "github.com/teknik-github/kcli"

// version is settable at build time (-ldflags "-X .../version.version=v0.2.0")
// for a plain `go build`, which otherwise has no version stamp. A `go install
// module@tag` build fills the same value in through the build info below, so
// the ldflag is only needed for local builds that want a real version string.
var version = ""

// Current returns the running version. It prefers the ldflag, then the module
// version recorded by `go install module@tag` (ReadBuildInfo), and finally
// "(devel)" for a plain local build with neither.
func Current() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

// info is the JSON the module proxy returns for the @latest endpoint.
type info struct {
	Version string
	Time    time.Time
}

// Latest asks the Go module proxy for the highest released version of the
// module. It is best-effort: any network/parse failure returns an error the
// caller is expected to swallow (an update check must never disrupt the app).
func Latest(ctx context.Context) (string, error) {
	url := fmt.Sprintf("https://proxy.golang.org/%s/@latest", Module)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("proxy returned %s", resp.Status)
	}
	var v info
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.Version == "" {
		return "", fmt.Errorf("proxy returned no version")
	}
	return v.Version, nil
}

// IsNewer reports whether latest is a strictly higher release than current.
// It returns false whenever either side does not parse as a clean vX.Y.Z
// release tag — a "(devel)" or pseudo-version current build must not be nagged
// to "update", since the proxy's highest tag may well be older than it.
func IsNewer(latest, current string) bool {
	lv, ok1 := parse(latest)
	cv, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	return lv.compare(cv) > 0
}

type semver struct{ major, minor, patch int }

// parse accepts a clean release tag "vX.Y.Z" (leading "v" optional, build
// metadata after "+" ignored). Anything with a prerelease/pseudo suffix after
// "-" is rejected, so only proper releases ever trigger an update prompt.
func parse(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if strings.ContainsRune(s, '-') {
		return semver{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		nums[i] = n
	}
	return semver{nums[0], nums[1], nums[2]}, true
}

func (a semver) compare(b semver) int {
	switch {
	case a.major != b.major:
		return a.major - b.major
	case a.minor != b.minor:
		return a.minor - b.minor
	default:
		return a.patch - b.patch
	}
}
