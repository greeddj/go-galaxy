package cleanup

import "github.com/greeddj/go-galaxy/internal/galaxy/requirements"

// loadRequirements reads requirements for cleanup scope.
func loadRequirements(path, defaultSource string) ([]requirements.CollectionRequirement, error) {
	reqs, _, err := requirements.LoadCollections(path, defaultSource)
	return reqs, err
}
