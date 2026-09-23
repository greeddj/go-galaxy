package exitcode

import (
	"fmt"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// roleExitCases pins the exit class of every role sentinel, one row per
// sentinel; a role sentinel added to helpers needs a row here.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var roleExitCases = []exitCase{
	{name: "roles not a list", err: helpers.ErrInvalidRolesList, wantCode: ExitUsage},
	{name: "invalid role entry", err: helpers.ErrInvalidRoleEntry, wantCode: ExitUsage},
	{name: "invalid role name", err: helpers.ErrInvalidRoleName, wantCode: ExitUsage},
	{name: "invalid role install name", err: helpers.ErrInvalidRoleInstallName, wantCode: ExitUsage},
	{name: "invalid role version", err: helpers.ErrInvalidRoleVersion, wantCode: ExitUsage},
	{name: "unsupported role source", err: helpers.ErrUnsupportedRoleSource, wantCode: ExitUsage},
	{name: "unsupported role scm", err: helpers.ErrUnsupportedRoleScm, wantCode: ExitUsage},
	{name: "role include", err: helpers.ErrUnsupportedRoleInclude, wantCode: ExitUsage},
	{name: "duplicate role requirement", err: helpers.ErrDuplicateRoleRequirement, wantCode: ExitUsage},
	{name: "no v1 role api", err: helpers.ErrGalaxyRoleAPIUnavailable, wantCode: ExitUsage},
	{name: "invalid v1 record", err: helpers.ErrGalaxyRoleInvalid, wantCode: ExitUsage},
	{name: "role meta not found", err: helpers.ErrRoleMetaNotFound, wantCode: ExitUsage},
	{name: "role meta invalid", err: helpers.ErrRoleMetaInvalid, wantCode: ExitUsage},
	{name: "role not found", err: helpers.ErrRoleNotFound, wantCode: ExitResolution},
	{name: "role version not found", err: helpers.ErrRoleVersionNotFound, wantCode: ExitResolution},
	{name: "role versions incomparable", err: helpers.ErrRoleVersionsIncomparable, wantCode: ExitResolution},
	{name: "role directory foreign", err: helpers.ErrRoleDirectoryForeign, wantCode: ExitInstall},
	{name: "role artifact identity mismatch", err: helpers.ErrRoleArtifactIdentityMismatch, wantCode: ExitIntegrity},
}

// TestRoleSentinelsAreAllClassified checks every role sentinel both bare and
// wrapped, and refuses the generic fallback for any of them.
func TestRoleSentinelsAreAllClassified(t *testing.T) {
	t.Parallel()
	for _, tt := range roleExitCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FromError(tt.err); got != tt.wantCode {
				t.Fatalf("FromError(bare) = %d, want %d", got, tt.wantCode)
			}
			wrapped := fmt.Errorf("roles[0]: %w", tt.err)
			if got := FromError(wrapped); got != tt.wantCode {
				t.Fatalf("FromError(wrapped) = %d, want %d", got, tt.wantCode)
			}
			if tt.wantCode == ExitError {
				t.Fatalf("a role sentinel must never classify as the generic fallback")
			}
		})
	}
}
