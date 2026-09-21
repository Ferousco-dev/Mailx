// Package buildinfo reports the build identity injected at link time:
//
//	go build -ldflags "-X github.com/Ferousco-dev/mailx/internal/buildinfo.Version=v1.2.3 \
//	                   -X github.com/Ferousco-dev/mailx/internal/buildinfo.Commit=abc1234"
//
// Unset values fall back to "dev" and "unknown"; no release version is invented.
package buildinfo

// Set by -ldflags -X. Exported only so the linker can reach them.
var (
	Version = ""
	Commit  = ""
)

// Info is the build identity of this binary.
type Info struct {
	Version string
	Commit  string
}

// Get returns the injected values, or the safe fallbacks.
func Get() Info {
	i := Info{Version: Version, Commit: Commit}
	if i.Version == "" {
		i.Version = "dev"
	}
	if i.Commit == "" {
		i.Commit = "unknown"
	}
	return i
}
