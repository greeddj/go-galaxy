package helpers

import (
	"errors"
	"fmt"
	"net/url"
)

// TransportURLError renders a failed HTTP request without the capability its
// URL carries, keeping the transport failure reachable through Unwrap.
type TransportURLError struct {
	err     *url.Error
	display string
}

// CutTransportURL re-renders a *url.Error from http.Client.Do over rawURL with
// userinfo and query cut, since net/http masks only the password; any other
// error is returned untouched. Classifiers must use errors.Is or errors.As.
func CutTransportURL(rawURL string, err error) error {
	urlErr, ok := errors.AsType[*url.Error](err)
	if !ok {
		return err
	}

	return &TransportURLError{err: urlErr, display: WithoutCredentials(rawURL)}
}

// Error renders operation, the cut URL the caller asked for (never a redirect
// hop's) and the inner cause; the cause is printed as is, so no RoundTripper
// this program installs may put an uncut URL into its own error.
func (e *TransportURLError) Error() string {
	return fmt.Sprintf("%s %q: %s", e.err.Op, e.display, e.err.Err)
}

// Unwrap exposes the original *url.Error, so every errors.Is and errors.As
// caller downstream classifies exactly what it classified before.
func (e *TransportURLError) Unwrap() error {
	return e.err
}
