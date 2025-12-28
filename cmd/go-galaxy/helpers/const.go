package helpers

import "time"

const (
	dirSuffix                   = ".cache/go-galaxy"
	defaultHomeDir              = "/root"
	defaultTimeout              = 30 * time.Second
	defaultServerURL            = "https://galaxy.ansible.com"
	defaultCollectionsPath      = ".collections"
	defaultRequirementsFilePath = "requirements.yml"
	defaultAnsibleConfigPath    = "ansible.cfg"
)
