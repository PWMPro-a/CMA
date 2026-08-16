// Package buildinfo exposes compile-time metadata shared by the manager and agent binaries.
package buildinfo

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)
