package commands

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// GitHub release metadata — hardcoded to the upstream repo. Centralised here
// so both `oc update` and the background update-check use the same source.
const (
	releasesLatestURL = "https://api.github.com/repos/diggerhq/opencomputer/releases/latest"
	releaseUserAgent  = "oc-cli"
)

type githubRelease struct {
	TagName string         `json:"tag_name"`
	HTMLURL string         `json:"html_url"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update the oc CLI to the latest release",
	Long: `Fetch the latest release from GitHub and replace the current binary in place.

Works on macOS and Linux (amd64/arm64). Requires write access to the binary's
current path; if oc is installed in a system-wide directory, re-run with sudo
or install into a user-writable location (e.g. ~/.local/bin).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		check, _ := cmd.Flags().GetBool("check")
		yes, _ := cmd.Flags().GetBool("yes")

		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		rel, err := fetchLatestRelease(ctx)
		if err != nil {
			return fmt.Errorf("check latest release: %w", err)
		}
		latest := strings.TrimPrefix(rel.TagName, "v")

		fmt.Printf("current:  v%s\n", Version)
		fmt.Printf("latest:   v%s\n", latest)
		fmt.Printf("release:  %s\n", rel.HTMLURL)

		if Version == "dev" {
			fmt.Println("\nthis is a dev build — skipping update. Build a tagged binary to enable self-update.")
			return nil
		}

		if compareVersions(Version, latest) >= 0 {
			fmt.Println("\nalready up to date.")
			return nil
		}

		if check {
			fmt.Printf("\nupdate available: run `oc update` to install v%s\n", latest)
			return nil
		}

		if !yes {
			fmt.Printf("\nReplace v%s with v%s? [Y/n] ", Version, latest)
			reader := bufio.NewReader(os.Stdin)
			line, _ := reader.ReadString('\n')
			resp := strings.ToLower(strings.TrimSpace(line))
			if resp != "" && resp != "y" && resp != "yes" {
				fmt.Println("aborted.")
				return nil
			}
		}

		if err := installRelease(ctx, rel, latest); err != nil {
			return err
		}
		fmt.Printf("✓ updated v%s → v%s\n", Version, latest)
		return nil
	},
}

// fetchLatestRelease queries the GitHub Releases API for the latest release
// of oc. Returns a populated githubRelease on success. Shared by the
// `update` command and the background update-check.
func fetchLatestRelease(ctx context.Context) (*githubRelease, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", releasesLatestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", releaseUserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("github api: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// installRelease picks the right asset for the current platform, verifies
// its SHA256 against the release's `checksums.txt`, and atomically replaces
// the running binary with the new one.
func installRelease(ctx context.Context, rel *githubRelease, version string) error {
	assetName := fmt.Sprintf("oc-%s-%s", runtime.GOOS, runtime.GOARCH)
	var binAsset, sumAsset *releaseAsset
	for i := range rel.Assets {
		a := &rel.Assets[i]
		switch a.Name {
		case assetName:
			binAsset = a
		case "checksums.txt":
			sumAsset = a
		}
	}
	if binAsset == nil {
		return fmt.Errorf("no release asset for %s/%s (looked for %q)", runtime.GOOS, runtime.GOARCH, assetName)
	}
	if sumAsset == nil {
		return errors.New("release is missing checksums.txt")
	}

	fmt.Printf("downloading %s (%.1f MB)...\n", binAsset.Name, float64(binAsset.Size)/(1<<20))
	binBytes, err := downloadBytes(ctx, binAsset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("download binary: %w", err)
	}

	sumBytes, err := downloadBytes(ctx, sumAsset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	expected, ok := parseChecksum(string(sumBytes), assetName)
	if !ok {
		return fmt.Errorf("checksums.txt missing entry for %s", assetName)
	}
	h := sha256.Sum256(binBytes)
	if got := hex.EncodeToString(h[:]); got != expected {
		return fmt.Errorf("checksum mismatch: got %s, expected %s", got, expected)
	}

	currentPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find current binary: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(currentPath); rerr == nil {
		currentPath = resolved
	}

	// Write to <path>.new in the same directory so os.Rename is atomic.
	tmpPath := currentPath + ".new"
	if err := os.WriteFile(tmpPath, binBytes, 0o755); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("no write access to %s (re-run with sudo or install into a user-writable path)", filepath.Dir(currentPath))
		}
		return fmt.Errorf("write new binary (%s): %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, currentPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("replace %s: %w", currentPath, err)
	}
	return nil
}

func downloadBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", releaseUserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http %s for %s", resp.Status, url)
	}
	return io.ReadAll(resp.Body)
}

// parseChecksum parses a shasum-style "<hash>  <filename>" file and returns
// the hex digest for the given filename. Accepts the "*filename" binary-mode
// prefix that GNU shasum emits in some configurations.
func parseChecksum(body, name string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		fname := strings.TrimPrefix(fields[1], "*")
		if fname == name {
			return fields[0], true
		}
	}
	return "", false
}

// compareVersions compares dotted numeric versions ("0.5.0.67"). Returns
// -1 if a<b, 0 if equal, +1 if a>b. Missing segments are treated as 0 so
// "0.5.0" < "0.5.0.1". Non-numeric segments sort before numeric ones.
func compareVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func init() {
	updateCmd.Flags().Bool("check", false, "Check for an update without installing")
	updateCmd.Flags().Bool("yes", false, "Skip confirmation prompt")
	rootCmd.AddCommand(updateCmd)
}
