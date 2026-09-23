// Package buildinfo renders the CLI's version string. Link-time ldflags are
// authoritative; a build without them reports the module and VCS metadata the
// toolchain embedded, never anything fetched off the machine.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version returns the formatted version string. Fields the ldflags left empty
// (a plain `go build` or `go run`) are recovered by fillFromBuildInfo; the
// version comes first, which the action's `--version` parse relies on.
func Version(version, commit, date, builtBy string) string {
	if version == "" || commit == "" || date == "" {
		version, commit, date = fillFromBuildInfo(version, commit, date)
	}
	if version == "" {
		version = defaultVersion
	}

	if builtBy == "" {
		builtBy = defaultBuilder
	}

	return formatVersion(version, commit, date, builtBy)
}

// formatVersion renders already-resolved fields into the display string, split
// out so its branches are testable apart from fillFromBuildInfo's environment.
func formatVersion(version, commit, date, builtBy string) string {
	switch {
	case date != "" && commit != "":
		return fmt.Sprintf("%s (commit %s, built by %s @ %s) // %s", version, commit, builtBy, date, runtime.Version())
	case date == "" && commit != "":
		return fmt.Sprintf("%s (commit %s, built by %s) // %s", version, commit, builtBy, runtime.Version())
	case date != "" && commit == "":
		return fmt.Sprintf("%s (built by %s @ %s) // %s", version, builtBy, date, runtime.Version())
	default:
		return fmt.Sprintf("%s (built by %s) // %s", version, builtBy, runtime.Version())
	}
}

// fillFromBuildInfo fills any of version, commit, date that are empty from
// runtime/debug.ReadBuildInfo, leaving already-set (ldflags-provided) values
// untouched. It is a pure, network-free fallback for dev builds.
func fillFromBuildInfo(version, commit, date string) (string, string, string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, commit, date
	}

	if version == "" {
		version = info.Main.Version
	}
	if commit == "" || date == "" {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "" {
					commit = setting.Value
				}
			case "vcs.time":
				if date == "" {
					date = setting.Value
				}
			}
		}
	}
	return version, commit, date
}
