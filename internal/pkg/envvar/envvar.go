// Package envvar reads settings from the environment.
package envvar

import (
	"os"
	"strings"
)

// Get returns the environment variable key without a trailing slash, or
// fallback if it is unset.
func Get(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimRight(v, "/")
	}
	return fallback
}
