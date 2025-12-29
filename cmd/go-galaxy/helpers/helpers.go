package helpers

import (
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
		builtBy = "go"
	}

	switch {
	case date != "" && commit != "":
		return fmt.Sprintf("%s (commit: %s, built: %s by %s) // %s", version, commit, date, builtBy, runtime.Version())
	case date != "" && commit == "":
		return fmt.Sprintf("%s (built: %s by %s) // %s", version, date, builtBy, runtime.Version())
	case date == "" && commit != "":
		return fmt.Sprintf("%s (commit: %s, built by %s) // %s", version, commit, builtBy, runtime.Version())
	default:
		return fmt.Sprintf("%s (built by %s) // %s", version, builtBy, runtime.Version())
	}
}

// latestTag fetches the latest release tag from GitHub.
func latestTag() string {
	client := &http.Client{Timeout: time.Second}
	req, err := http.NewRequest(http.MethodGet, latestVersionURL, http.NoBody)
	if err != nil {
		return "latest"
	}

	req.Header.Set("User-Agent", "go-galaxy")

	resp, err := client.Do(req)
	if err != nil {
		return "latest"
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "latest"
	}

	var payload struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "latest"
	}
	if payload.Tag == "" {
		return "latest"
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
