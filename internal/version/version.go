// Package version carries build metadata stamped in via -ldflags.
package version

import (
	"fmt"
	"runtime"
)

// Populated at build time by the Makefile. Defaults keep `go run` usable.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// String renders the full version banner shown by `kubespin --version`.
func String() string {
	return fmt.Sprintf("kubespin %s (commit %s, built %s, %s/%s, %s)",
		Version, Commit, BuildDate, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
