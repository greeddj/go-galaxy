package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Version returns the formatted version string for the application.
func Version(version, commit, date, builtBy string) string {
	if version == "" {
		version = latestTag()
	}

	if builtBy == "" {
		builtBy = defaultBuilder
	}

	switch {
	case date != "" && commit != "":
		return fmt.Sprintf("%s (commit %s, built by %s @ %s) // %s", version, commit, builtBy, date, runtime.Version())
	case date == "" && commit != "":
		return fmt.Sprintf("%s (commit %s, built by %s) // %s", version, commit, builtBy, runtime.Version())
	case date != "" && commit == "":
		return fmt.Sprintf("%s (built by %s @ %s) // %s", version, builtBy, date, runtime.Version())
	default:
		return fmt.Sprintf("%s (built by %s) // %s", version, builtBy, runtime.Version())
	}
}

// latestTag fetches the latest release tag from GitHub.
func latestTag() string {
	client := &http.Client{Timeout: time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, latestVersionURL, http.NoBody)
	if err != nil {
		return defaultVersion
	}

	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return defaultVersion
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return defaultVersion
	}

	var payload struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return defaultVersion
	}
	if payload.Tag == "" {
		return defaultVersion
	}
	return payload.Tag
}

// defaultCacheDir returns the default cache directory path.
func defaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(defaultHomeDir, dirSuffix)
	}
	return filepath.Join(home, dirSuffix)
}
