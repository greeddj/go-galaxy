package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// requirementsFileFlagName is the flag RequirementsPath and MigrateSourcePath
// read; a command that mounts no flag by this name gets "" and no file system
// access at all.
const requirementsFileFlagName = "requirements-file"

// RequirementsPath picks the run's requirements file: a set flag or variable
// whose name ends in .yml, .yaml or .toml, else discovery; the warning is ""
// or a printable line, and a set value is never Stat'ed here.
func RequirementsPath(c *cli.Command) (string, string, error) {
	if !flagMounted(c, requirementsFileFlagName) {
		return "", "", nil
	}
	// An exported-empty variable counts as set, so its "" is refused here
	// rather than handed on as a path; discovery never outranks a source.
	if c.IsSet(requirementsFileFlagName) {
		path := c.String(requirementsFileFlagName)
		if !hasRequirementsFileExtension(path) {
			return "", "", fmt.Errorf("%w: %q", helpers.ErrRequirementsFileName, path)
		}
		return path, "", nil
	}
	path, warning := discoverRequirementsPath()
	return path, warning, nil
}

// MigrateSourcePath returns the requirements.yml migrate reads: -r as given,
// else requirements.yml, never discovery and no variable. A name not ending in
// .yml or .yaml, any case, a .toml one and "" included, is ErrMigrateSourceName.
func MigrateSourcePath(c *cli.Command) (string, error) {
	path := c.String(requirementsFileFlagName)
	ext := filepath.Ext(path)
	switch {
	case strings.EqualFold(ext, ".yml") || strings.EqualFold(ext, ".yaml"):
		return path, nil
	case strings.EqualFold(ext, ".toml"):
		return "", fmt.Errorf("%w: %q is a galaxy.toml already", helpers.ErrMigrateSourceName, path)
	default:
		return "", fmt.Errorf("%w: %q", helpers.ErrMigrateSourceName, path)
	}
}

// hasRequirementsFileExtension reports whether path ends in .yml, .yaml or
// .toml, case ignored as projectfile.IsTOMLPath ignores it, so /dev/stdin, a
// <(...) descriptor and "" fail.
func hasRequirementsFileExtension(path string) bool {
	ext := filepath.Ext(path)
	return strings.EqualFold(ext, ".yml") || strings.EqualFold(ext, ".yaml") || strings.EqualFold(ext, ".toml")
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
