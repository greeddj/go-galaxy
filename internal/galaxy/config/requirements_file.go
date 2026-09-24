package config

import (
	"fmt"
	"os"
	"slices"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// requirementsFileFlagName is the flag RequirementsPath reads; a command that
// mounts no flag by this name gets "" and no file system access at all.
const requirementsFileFlagName = "requirements-file"

// RequirementsPath picks the run's requirements file: a set flag or variable
// verbatim, else ./galaxy.toml when it is a regular file, else the relative
// name requirements.yml without a Stat; the warning is "" or a printable line.
func RequirementsPath(c *cli.Command) (string, string) {
	if !flagMounted(c, requirementsFileFlagName) {
		return "", ""
	}
	// An exported-empty variable counts as set and yields "", exactly as the
	// flag did before discovery existed; discovery never outranks a source.
	if c.IsSet(requirementsFileFlagName) {
		return c.String(requirementsFileFlagName), ""
	}
	return discoverRequirementsPath()
}

// discoverRequirementsPath applies the unset-flag rule: a candidate stays
// relative, like cwdAnsibleCfgName, so it resolves against the working
// directory at open time; a world-writable directory is deliberately no bar.
func discoverRequirementsPath() (string, string) {
	tomlExists, tomlRegular := regularFileExists(helpers.RequirementsTOMLName)
	switch {
	case tomlRegular:
		if _, yamlRegular := regularFileExists(helpers.RequirementsYAMLName); yamlRegular {
			return helpers.RequirementsTOMLName, fmt.Sprintf(
				"%[1]s and %[2]s are both present in the current directory; using %[1]s and ignoring %[2]s "+
					"(name one with --requirements-file to choose)",
				helpers.RequirementsTOMLName, helpers.RequirementsYAMLName)
		}
		return helpers.RequirementsTOMLName, ""
	case tomlExists:
		return helpers.RequirementsYAMLName, fmt.Sprintf(
			"./%s is not a regular file and is ignored; reading %s instead "+
				"(name a file with --requirements-file to read it)",
			helpers.RequirementsTOMLName, helpers.RequirementsYAMLName)
	default:
		return helpers.RequirementsYAMLName, ""
	}
}

// flagMounted reports whether c itself declares a flag answering to name.
// IsSet alone cannot tell an unmounted flag from an unset one, and only a
// mounted flag licenses the Stat calls discovery makes.
func flagMounted(c *cli.Command, name string) bool {
	for _, flag := range c.Flags {
		if slices.Contains(flag.Names(), name) {
			return true
		}
	}
	return false
}

// regularFileExists reports whether path exists and whether it is a regular
// file, following a symlink; a failed Stat reads as absent, so the caller
// falls through to the next candidate rather than opening anything.
func regularFileExists(path string) (bool, bool) {
	// #nosec G703 -- path is a fixed relative candidate name in the user's own
	// working directory, never a flag value; this is a read-only existence
	// check that never opens or writes the file.
	info, err := os.Stat(path)
	if err != nil {
		return false, false
	}
	return true, info.Mode().IsRegular()
}
