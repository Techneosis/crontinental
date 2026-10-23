package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// version is set by release builds (-ldflags "-X main.version=v1.2.3").
// Anything else reports the commit it was built from, when Go recorded one.
var version = ""

func versionString() string {
	v := version
	if v == "" {
		v = "dev"
		if bi, ok := debug.ReadBuildInfo(); ok {
			rev, dirty := "", false
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					rev = s.Value
				case "vcs.modified":
					dirty = s.Value == "true"
				}
			}
			if len(rev) >= 7 {
				v = "dev-" + rev[:7]
				if dirty {
					v += "-dirty"
				}
			}
		}
	}
	return fmt.Sprintf("crontinental %s (%s/%s)", v, runtime.GOOS, runtime.GOARCH)
}
