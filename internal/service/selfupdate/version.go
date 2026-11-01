// Package selfupdate replaces the running grokbot-tunnel with another release
// and restarts its service.
package selfupdate

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/githubrelease"
)

// devSuffix marks versions from git describe between or after tags.
var devSuffix = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+(-dirty)?$|-dirty$`)

// IsRelease reports whether version is a release tag rather than a
// development build.
func IsRelease(version string) bool {
	return githubrelease.IsTag(version) && !devSuffix.MatchString(version) && semver.IsValid(version)
}

// Normalize turns "latest" or a version, with or without the leading v, into
// "latest" or a tag.
func Normalize(version string) (string, error) {
	if version == "latest" {
		return version, nil
	}
	v := version
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !githubrelease.IsTag(v) {
		return "", fmt.Errorf("invalid version %q: want latest or a release such as v1.2.3", version)
	}
	return v, nil
}

// Compare returns -1, 0 or +1 as release a is older than, equal to or newer
// than b.
func Compare(a, b string) int {
	return semver.Compare(a, b)
}
