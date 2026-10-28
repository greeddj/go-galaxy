package helpers

import (
	"io/fs"
	"net/http"
	"testing"
)

// TestFetchRetryPolicyCarriesTheSharedFetchBudget pins that every Galaxy GET
// and artifact download retries under the Fetch* constants and nothing else.
func TestFetchRetryPolicyCarriesTheSharedFetchBudget(t *testing.T) {
	t.Parallel()
	want := RetryPolicy{Base: FetchRetryBackoffBase, Cap: FetchRetryBackoffCap, MaxAttempts: FetchRetryMaxAttempts}
	if got := FetchRetryPolicy(); got != want {
		t.Fatalf("FetchRetryPolicy() = %+v, want %+v", got, want)
	}
}

// TestDefaultInstallWorkersClampsToItsRange pins the floor, the pass-through
// middle and the memory ceiling of the install pool's default.
func TestDefaultInstallWorkersClampsToItsRange(t *testing.T) {
	t.Parallel()
	cases := []struct{ procs, want int }{
		{procs: 0, want: MinDefaultInstallWorkers},
		{procs: 1, want: MinDefaultInstallWorkers},
		{procs: MinDefaultInstallWorkers + 1, want: MinDefaultInstallWorkers + 1},
		{procs: MaxDefaultInstallWorkers, want: MaxDefaultInstallWorkers},
		{procs: MaxDefaultInstallWorkers * 4, want: MaxDefaultInstallWorkers},
	}
	for _, tc := range cases {
		if got := DefaultInstallWorkers(tc.procs); got != tc.want {
			t.Errorf("DefaultInstallWorkers(%d) = %d, want %d", tc.procs, got, tc.want)
		}
	}
}

// TestMaxAcceptedInstallWorkersHasAFloorButNoCeiling pins that the accepted
// --workers bound follows the permitted CPU upward, past the default's cap.
func TestMaxAcceptedInstallWorkersHasAFloorButNoCeiling(t *testing.T) {
	t.Parallel()
	cases := []struct{ procs, want int }{
		{procs: 1, want: MinDefaultInstallWorkers},
		{procs: MaxDefaultInstallWorkers * 4, want: MaxDefaultInstallWorkers * 4},
	}
	for _, tc := range cases {
		if got := MaxAcceptedInstallWorkers(tc.procs); got != tc.want {
			t.Errorf("MaxAcceptedInstallWorkers(%d) = %d, want %d", tc.procs, got, tc.want)
		}
	}
}

// TestDefaultDownloadWorkersScalesPerCPUWithinItsRange pins the per-CPU
// multiplier and the clamp to [MinDefaultDownloadWorkers, MaxDefaultDownloadWorkers].
func TestDefaultDownloadWorkersScalesPerCPUWithinItsRange(t *testing.T) {
	t.Parallel()
	scaled := MinDefaultDownloadWorkers/DownloadWorkersPerCPU + 1
	cases := []struct{ procs, want int }{
		{procs: 1, want: MinDefaultDownloadWorkers},
		{procs: scaled, want: scaled * DownloadWorkersPerCPU},
		{procs: MaxDefaultDownloadWorkers, want: MaxDefaultDownloadWorkers},
	}
	for _, tc := range cases {
		if got := DefaultDownloadWorkers(tc.procs); got != tc.want {
			t.Errorf("DefaultDownloadWorkers(%d) = %d, want %d", tc.procs, got, tc.want)
		}
	}
}

// TestReadOnlyPermClearsOnlyTheWriteBits pins that read, execute and type bits
// survive while every write bit is cleared.
func TestReadOnlyPermClearsOnlyTheWriteBits(t *testing.T) {
	t.Parallel()
	cases := []struct{ perm, want fs.FileMode }{
		{perm: 0o755, want: 0o555},
		{perm: 0o644, want: 0o444},
		{perm: 0o200, want: 0},
		{perm: 0o444, want: 0o444},
		{perm: fs.ModeSetuid | 0o777, want: fs.ModeSetuid | 0o555},
	}
	for _, tc := range cases {
		if got := ReadOnlyPerm(tc.perm); got != tc.want {
			t.Errorf("ReadOnlyPerm(%v) = %v, want %v", tc.perm, got, tc.want)
		}
	}
}

// TestIsRetryableHTTPStatusAcceptsOnlyTransientFailures pins the shared retry
// set: throttling and the transient 5xx statuses, never a client error or 501.
func TestIsRetryableHTTPStatusAcceptsOnlyTransientFailures(t *testing.T) {
	t.Parallel()
	retryable := []int{
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	}
	for _, status := range retryable {
		if !IsRetryableHTTPStatus(status) {
			t.Errorf("IsRetryableHTTPStatus(%d) = false, want true", status)
		}
	}
	terminal := []int{
		http.StatusOK, http.StatusNotModified, http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusForbidden, http.StatusNotFound, http.StatusRequestTimeout, http.StatusConflict,
		http.StatusPreconditionFailed, http.StatusNotImplemented,
	}
	for _, status := range terminal {
		if IsRetryableHTTPStatus(status) {
			t.Errorf("IsRetryableHTTPStatus(%d) = true, want false", status)
		}
	}
}
