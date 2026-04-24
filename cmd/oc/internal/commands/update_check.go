package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
)

// Background update-check: once every 24h at most, cached on disk. Emits
// a one-line nag to stderr when a newer release is available. Silent on
// dev builds, non-TTY stderr, or when OC_NO_UPDATE_CHECK is set.

const updateCheckInterval = 24 * time.Hour
const updateCheckTimeout = 2 * time.Second

type updateCheckCache struct {
	CheckedAt time.Time `json:"checked_at"`
	LatestTag string    `json:"latest_tag"`
}

// maybePromptUpdate runs after every non-update command. Cheap in the
// common case (reads a small JSON file); does a network call only once
// per 24h. Any error bails silently — the CLI must never block or warn
// on a failed version check.
func maybePromptUpdate() {
	if Version == "dev" {
		return
	}
	if os.Getenv("OC_NO_UPDATE_CHECK") != "" {
		return
	}
	// Only nag humans, never scripts/CI. Stderr is the nag channel because
	// callers often parse stdout; TTY-detecting stderr keeps pipelines clean.
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}

	path, err := updateCheckCachePath()
	if err != nil {
		return
	}
	cached, _ := readUpdateCheckCache(path)

	var latestTag string
	if cached != nil && time.Since(cached.CheckedAt) < updateCheckInterval {
		latestTag = cached.LatestTag
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
		defer cancel()
		rel, err := fetchLatestRelease(ctx)
		if err != nil {
			return
		}
		latestTag = rel.TagName
		_ = writeUpdateCheckCache(path, &updateCheckCache{
			CheckedAt: time.Now(),
			LatestTag: latestTag,
		})
	}

	latest := strings.TrimPrefix(latestTag, "v")
	if latest == "" {
		return
	}
	if compareVersions(Version, latest) < 0 {
		fmt.Fprintf(os.Stderr,
			"\nA new oc release is available: v%s (current: v%s). Run `oc update` to install. Disable this check with OC_NO_UPDATE_CHECK=1.\n",
			latest, Version)
	}
}

func updateCheckCachePath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	full := filepath.Join(dir, "opencomputer")
	if err := os.MkdirAll(full, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(full, "update-check.json"), nil
}

func readUpdateCheckCache(path string) (*updateCheckCache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c updateCheckCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func writeUpdateCheckCache(path string, c *updateCheckCache) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
