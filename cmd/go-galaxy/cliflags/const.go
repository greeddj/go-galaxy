package cliflags

import (
	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	dirSuffix      = ".cache/go-galaxy"
	defaultHomeDir = "/root"
	// defaultTimeout is only what --timeout advertises; it is the constant the
	// config layer falls back to, so the help cannot disagree with the behavior.
	defaultTimeout              = galaxyhelpers.FetchDefaultTimeout
	defaultServerURL            = "https://galaxy.ansible.com"
	defaultCollectionsPath      = ".collections"
	defaultRolesPath            = ".roles"
	defaultRequirementsFilePath = "requirements.yml"
	// envRequirementsFileAnsible is a go-galaxy extension in ansible's namespace
	// (ansible-core defines no such option). Pipelines set it, and deleting it
	// would silently install whatever requirements.yml the directory holds.
	envRequirementsFileAnsible = "ANSIBLE_GALAXY_REQUIREMENTS_FILE"
)
