package api_test

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestFiles_UploadDownload exercises the signed-URL file path: get an upload
// URL, PUT content to it, get a download URL, GET it back, verify byte match.
// Also confirms exec sees the same content (proves the upload landed in the
// sandbox's workspace, not just a server-side cache).
func TestFiles_UploadDownload(t *testing.T) {
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 1 {
		t.Skipf("%s<1, skipping file ops test", envWorkers)
	}
	c := newClient(t)
	sandboxID, _ := createReadySandbox(t, c, map[string]any{
		"cpuCount": 1, "memoryMB": 1024, "diskMB": 20480, "timeout": 300,
	})

	const path = "/home/sandbox/upload-test.txt"
	payload := []byte("hello-ci-upload " + strconv.FormatInt(int64(len(path)), 10))

	// Get upload URL
	var uploadResp struct {
		URL string `json:"url"`
	}
	code, err := c.do(t, http.MethodPost,
		"/api/sandboxes/"+sandboxID+"/files/upload-url",
		map[string]any{"path": path, "expiresIn": 60}, &uploadResp)
	if err != nil || code/100 != 2 || uploadResp.URL == "" {
		t.Fatalf("upload-url: code=%d err=%v resp=%+v", code, err, uploadResp)
	}

	// PUT content to the upload URL. Signed URLs are HMAC-authenticated so
	// they don't need our X-API-Key header.
	uploadURL := uploadResp.URL
	if strings.HasPrefix(uploadURL, "/") {
		uploadURL = c.baseURL + uploadURL
	}
	req, err := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build PUT: %v", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT upload: status=%d body=%q", resp.StatusCode, body)
	}

	// Get download URL
	var downloadResp struct {
		URL string `json:"url"`
	}
	code, err = c.do(t, http.MethodPost,
		"/api/sandboxes/"+sandboxID+"/files/download-url",
		map[string]any{"path": path, "expiresIn": 60}, &downloadResp)
	if err != nil || code/100 != 2 || downloadResp.URL == "" {
		t.Fatalf("download-url: code=%d err=%v resp=%+v", code, err, downloadResp)
	}

	downloadURL := downloadResp.URL
	if strings.HasPrefix(downloadURL, "/") {
		downloadURL = c.baseURL + downloadURL
	}
	req, err = http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		t.Fatalf("build GET: %v", err)
	}
	resp, err = c.http.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET download: status=%d body=%q", resp.StatusCode, raw)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, payload) {
		t.Errorf("download bytes mismatch: want %q, got %q", payload, got)
	}

	// Cross-check via exec — the file should be in the workspace.
	body2 := map[string]any{"cmd": "cat", "args": []string{path}, "timeout": 10}
	var r struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
	}
	code, err = c.do(t, http.MethodPost, "/api/sandboxes/"+sandboxID+"/exec/run", body2, &r)
	if err != nil || code != http.StatusOK || r.ExitCode != 0 {
		t.Fatalf("exec cat: code=%d err=%v exit=%d", code, err, r.ExitCode)
	}
	if !strings.Contains(r.Stdout, string(payload)) {
		t.Errorf("exec sees different content: want %q, got %q", payload, r.Stdout)
	}
}
