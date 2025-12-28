package collections

import "github.com/greeddj/go-galaxy/internal/galaxy/requirements"

// loadRequirements parses collection requirements into internal structs.
func loadRequirements(path, defaultSource string) ([]collection, bool, error) {
	reqs, rolesFound, err := requirements.LoadCollections(path, defaultSource)
	if err != nil {
		return nil, false, err
	}
	collections := make([]collection, 0, len(reqs))
	for _, req := range reqs {
		collections = append(collections, collection{
			Namespace:  req.Namespace,
			Name:       req.Name,
			Version:    req.Version,
			Source:     req.Source,
			Signatures: req.Signatures,
			Constraint: req.Version,
			Type:       req.Type,
		})
	}
	return collections, rolesFound, nil
}
