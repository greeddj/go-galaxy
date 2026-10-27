package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// applyRefreshSelection reads lock's --collection and --role into cfg, each
// element trimmed and judged; either flag without --refresh is refused, since
// alone it moves nothing.
func applyRefreshSelection(cfg *Config, c *cli.Command) error {
	collections, err := refreshSelection(c.StringSlice("collection"), "--collection", isRefreshCollectionName)
	if err != nil {
		return err
	}
	roles, err := refreshSelection(c.StringSlice("role"), "--role", helpers.IsRoleInstallName)
	if err != nil {
		return err
	}
	if (len(collections) > 0 || len(roles) > 0) && !cfg.Refresh {
		return helpers.ErrRefreshSelectionWithoutRefresh
	}
	cfg.RefreshCollections, cfg.RefreshRoles = collections, roles
	return nil
}

// refreshSelection trims, judges, sorts and deduplicates one flag's values;
// an empty element, as "a,,b" or --collection= gives, is refused.
func refreshSelection(values []string, flag string, valid func(string) bool) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(values))
	for _, value := range values {
		name := strings.TrimSpace(value)
		if !valid(name) {
			return nil, fmt.Errorf("%w: %s %q", helpers.ErrInvalidRefreshSelection, flag, helpers.TruncateForMessage(value))
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// isRefreshCollectionName admits every name a lockfile entry may carry: the
// Galaxy alphabet, and the wider one a url collection's MANIFEST.json may use.
func isRefreshCollectionName(name string) bool {
	namespace, collection, ok := helpers.SplitFQDN(name)
	return ok && helpers.IsURLCollectionNamePart(namespace) && helpers.IsURLCollectionNamePart(collection)
}
