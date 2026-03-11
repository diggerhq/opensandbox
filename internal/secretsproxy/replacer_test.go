package secretsproxy

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestProcessChunk_BasicReplacement(t *testing.T) {
	tokens := map[string]string{
		"osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "real-secret-value",
	}

	input := []byte(`{"Authorization": "Bearer osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	out, remainder := processChunk(input, tokens, true)

	expected := `{"Authorization": "Bearer real-secret-value"}`
	if string(out) != expected {
		t.Errorf("got %q, want %q", string(out), expected)
	}
	if remainder != nil {
		t.Errorf("expected nil remainder on flush, got %q", string(remainder))
	}
}

func TestProcessChunk_MultipleTokens(t *testing.T) {
	tokens := map[string]string{
		"osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "secret-1",
		"osb_sealed_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": "secret-2",
	}

	input := []byte(`key1=osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa&key2=osb_sealed_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`)
	out, _ := processChunk(input, tokens, true)

	expected := `key1=secret-1&key2=secret-2`
	if string(out) != expected {
		t.Errorf("got %q, want %q", string(out), expected)
	}
}

func TestProcessChunk_UnknownToken(t *testing.T) {
	tokens := map[string]string{
		"osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "secret-1",
	}

	// This token is not in the map — should pass through unchanged
	input := []byte(`osb_sealed_cccccccccccccccccccccccccccccccc`)
	out, _ := processChunk(input, tokens, true)

	if string(out) != string(input) {
		t.Errorf("unknown token should pass through: got %q, want %q", string(out), string(input))
	}
}

func TestProcessChunk_PartialPrefixAtEnd(t *testing.T) {
	tokens := map[string]string{
		"osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "secret",
	}

	// Data ends with a partial prefix "osb_seal" — should hold back
	input := []byte(`some data osb_seal`)
	out, remainder := processChunk(input, tokens, false)

	if string(out) != "some data " {
		t.Errorf("output got %q, want %q", string(out), "some data ")
	}
	if string(remainder) != "osb_seal" {
		t.Errorf("remainder got %q, want %q", string(remainder), "osb_seal")
	}
}

func TestProcessChunk_TokenSplitAcrossBoundary(t *testing.T) {
	tokens := map[string]string{
		"osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "secret",
	}

	// Split the token: first 17 chars in chunk 1 ("osb_sealed_" + 6 a's), remaining 26 a's in chunk 2
	chunk1 := []byte(`data osb_sealed_aaaaaa`)
	out1, remainder1 := processChunk(chunk1, tokens, false)

	if string(out1) != "data " {
		t.Errorf("chunk1 output got %q, want %q", string(out1), "data ")
	}
	// remainder1 should be "osb_sealed_aaaaaa" (partial token held back)
	if string(remainder1) != "osb_sealed_aaaaaa" {
		t.Errorf("remainder got %q, want %q", string(remainder1), "osb_sealed_aaaaaa")
	}

	// Second chunk: remainder + exactly 26 more a's to complete the 32-hex token + more data
	chunk2 := append(remainder1, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaa more")...) // 26 a's + " more"
	out2, _ := processChunk(chunk2, tokens, true)

	if string(out2) != "secret more" {
		t.Errorf("chunk2 output got %q, want %q", string(out2), "secret more")
	}
}

func TestStreamReplacer_SmallReads(t *testing.T) {
	tokens := map[string]string{
		"osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "SECRET",
	}

	input := `prefix osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa suffix`
	expected := `prefix SECRET suffix`

	// Use a tiny read buffer to stress chunk boundaries
	src := &smallReader{data: []byte(input), chunkSize: 7}
	r := newStreamReplacer(src, tokens)

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}

	if buf.String() != expected {
		t.Errorf("got %q, want %q", buf.String(), expected)
	}
}

func TestStreamReplacer_LargeBody(t *testing.T) {
	tokens := map[string]string{
		"osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "SECRET",
	}

	// Build a large body with a token buried in the middle
	padding := strings.Repeat("x", 100000)
	input := padding + "osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + padding
	expected := padding + "SECRET" + padding

	r := newStreamReplacer(strings.NewReader(input), tokens)

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}

	if buf.String() != expected {
		t.Errorf("output length: got %d, want %d", buf.Len(), len(expected))
	}
}

func TestStreamReplacer_NoTokens(t *testing.T) {
	tokens := map[string]string{}
	input := "no tokens here"

	r := newStreamReplacer(strings.NewReader(input), tokens)
	var buf bytes.Buffer
	io.Copy(&buf, r)

	if buf.String() != input {
		t.Errorf("got %q, want %q", buf.String(), input)
	}
}

func TestFindSafeFlushPoint(t *testing.T) {
	tests := []struct {
		data     string
		expected int
	}{
		{"no match here", 13},
		{"data ending with o", 17},          // "o" matches prefix[0]
		{"data ending with osb", 17},        // "osb" matches prefix[:3]
		{"data ending with osb_sealed", 17}, // "osb_sealed" matches prefix[:10]
		{"osb_s", 0},                        // partial prefix is all the data
	}

	for _, tt := range tests {
		got := findSafeFlushPoint([]byte(tt.data))
		if got != tt.expected {
			t.Errorf("findSafeFlushPoint(%q) = %d, want %d", tt.data, got, tt.expected)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	tests := []struct {
		host    string
		allowed []string
		want    bool
	}{
		{"api.anthropic.com", []string{"api.anthropic.com"}, true},
		{"evil.com", []string{"api.anthropic.com"}, false},
		{"api.anthropic.com", []string{"*.anthropic.com"}, true},
		{"anthropic.com", []string{"*.anthropic.com"}, false}, // *.x.com doesn't match x.com
		{"anything.com", []string{"*"}, true},
		{"anything.com", nil, false},
	}

	for _, tt := range tests {
		got := hostAllowed(tt.host, tt.allowed)
		if got != tt.want {
			t.Errorf("hostAllowed(%q, %v) = %v, want %v", tt.host, tt.allowed, got, tt.want)
		}
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		ip      string
		private bool
	}{
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.32.0.1", false},
		{"192.168.1.1", true},
		{"127.0.0.1", true},
		{"169.254.1.1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false},
	}

	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		got := isPrivateIP(ip)
		if got != tt.private {
			t.Errorf("isPrivateIP(%s) = %v, want %v", tt.ip, got, tt.private)
		}
	}
}

func TestShouldReplaceTokens(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		secretHosts []string
		want        bool
	}{
		{"nil secret hosts replaces all", "anything.com", nil, true},
		{"empty secret hosts replaces all", "anything.com", []string{}, true},
		{"exact match", "api.anthropic.com", []string{"api.anthropic.com"}, true},
		{"no match", "evil.com", []string{"api.anthropic.com"}, false},
		{"wildcard match", "api.anthropic.com", []string{"*.anthropic.com"}, true},
		{"wildcard no match", "evil.com", []string{"*.anthropic.com"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &Session{SecretHosts: tt.secretHosts}
			got := shouldReplaceTokens(tt.host, session)
			if got != tt.want {
				t.Errorf("shouldReplaceTokens(%q, %v) = %v, want %v", tt.host, tt.secretHosts, got, tt.want)
			}
		})
	}
}

func TestReplaceHeaderTokens(t *testing.T) {
	tokens := map[string]string{
		"osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "real-api-key",
		"osb_sealed_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": "real-bearer-token",
	}

	headers := http.Header{
		"Authorization": {"Bearer osb_sealed_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		"X-Api-Key":     {"osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"Content-Type":  {"application/json"},
	}

	replaceHeaderTokens(headers, tokens)

	if got := headers.Get("Authorization"); got != "Bearer real-bearer-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer real-bearer-token")
	}
	if got := headers.Get("X-Api-Key"); got != "real-api-key" {
		t.Errorf("X-Api-Key = %q, want %q", got, "real-api-key")
	}
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type should be unchanged, got %q", got)
	}
}

func TestReplaceHeaderTokens_MultipleValues(t *testing.T) {
	tokens := map[string]string{
		"osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "secret1",
	}

	headers := http.Header{
		"X-Custom": {
			"first osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa end",
			"no tokens here",
			"another osb_sealed_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa end",
		},
	}

	replaceHeaderTokens(headers, tokens)

	vals := headers["X-Custom"]
	if vals[0] != "first secret1 end" {
		t.Errorf("vals[0] = %q, want %q", vals[0], "first secret1 end")
	}
	if vals[1] != "no tokens here" {
		t.Errorf("vals[1] should be unchanged, got %q", vals[1])
	}
	if vals[2] != "another secret1 end" {
		t.Errorf("vals[2] = %q, want %q", vals[2], "another secret1 end")
	}
}

// smallReader returns data in small chunks to test boundary handling.
type smallReader struct {
	data      []byte
	offset    int
	chunkSize int
}

func (r *smallReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	end := r.offset + r.chunkSize
	if end > len(r.data) {
		end = len(r.data)
	}
	n := copy(p, r.data[r.offset:end])
	r.offset += n
	return n, nil
}
