package gitsource

import (
	"context"
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Offline is the Client an --offline run is wired with. Every method refuses
// with helpers.ErrOfflineMode before any transport, so no call site can reach
// a remote whatever it checked first, ssh included, which bypasses HTTP.
type Offline struct{}

var _ Client = Offline{}

// Advertise refuses even a commit ref, which needs no round trip: no call
// site advertises under --offline, so a call here is one that must not happen.
func (Offline) Advertise(_ context.Context, u URL, _ Ref, _ Credential) (string, string, error) {
	return "", "", offlineRefusal(u)
}

// Acquire refuses to fetch the repository req names.
func (Offline) Acquire(_ context.Context, req Request) (Result, error) {
	return Result{}, offlineRefusal(req.URL)
}

// AcquireRole refuses to fetch the repository req names.
func (Offline) AcquireRole(_ context.Context, req RoleRequest) (RoleResult, error) {
	return RoleResult{}, offlineRefusal(req.URL)
}

func offlineRefusal(u URL) error {
	return fmt.Errorf("%w: refusing to contact git repository %s", helpers.ErrOfflineMode, helpers.URLForMessage(u.String()))
}
