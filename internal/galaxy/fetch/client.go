package fetch

import (
	"net"
	"net/http"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// New creates a configured HTTP client with reasonable defaults.
func New(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   helpers.FetchDialContextTimeout,
				KeepAlive: helpers.FetchDialContextKeepAlive,
			}).DialContext,
			ForceAttemptHTTP2:     helpers.FetchForceAttemptHTTP2,
			MaxIdleConns:          helpers.FetchMaxIdleConns,
			MaxIdleConnsPerHost:   helpers.FetchMaxIdleConnsPerHost,
			IdleConnTimeout:       helpers.FetchIdleConnTimeout,
			TLSHandshakeTimeout:   helpers.FetchTLSHandshakeTimeout,
			ExpectContinueTimeout: helpers.FetchExpectContinueTimeout,
		},
	}
}
