// Package version exposes build-time version metadata.
package version

// Set via -ldflags at release time.
var (
	Version = "dev"
	Commit  = "none"
)

// String renders the human-readable version line.
func String() string {
	return Version + " (" + Commit + ")"
}
