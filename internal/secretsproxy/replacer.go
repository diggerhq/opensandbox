package secretsproxy

import (
	"bytes"
	"io"
)

const (
	tokenPrefix = "osb_sealed_"
	tokenHexLen = 32 // 16 random bytes hex-encoded
	tokenLen    = len(tokenPrefix) + tokenHexLen // 43
)

// streamReplacer wraps an io.Reader, replacing osb_sealed_xxx tokens with
// real secret values. It correctly handles tokens split across read boundaries
// by maintaining a small overlap buffer between reads.
type streamReplacer struct {
	src    io.Reader
	tokens map[string]string // sealed token → real value
	buf    []byte            // accumulated unprocessed bytes
	out    []byte            // processed bytes ready for Read()
	eof    bool
	tmp    []byte // reusable read buffer
}

// newStreamReplacer creates a replacer that reads from src and substitutes
// sealed tokens. If tokens is empty, reads pass through without buffering.
func newStreamReplacer(src io.Reader, tokens map[string]string) io.Reader {
	if len(tokens) == 0 {
		return src
	}
	return &streamReplacer{
		src:    src,
		tokens: tokens,
		tmp:    make([]byte, 32*1024),
	}
}

func (r *streamReplacer) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.eof {
			if len(r.buf) > 0 {
				// Flush remaining bytes — no more data coming
				r.out, _ = processChunk(r.buf, r.tokens, true)
				r.buf = nil
				if len(r.out) > 0 {
					break
				}
			}
			return 0, io.EOF
		}

		n, err := r.src.Read(r.tmp)
		if n > 0 {
			r.buf = append(r.buf, r.tmp[:n]...)
		}
		if err == io.EOF {
			r.eof = true
		} else if err != nil {
			return 0, err
		}

		// Process accumulated buffer
		var remainder []byte
		r.out, remainder = processChunk(r.buf, r.tokens, r.eof)
		r.buf = remainder
	}

	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

// processChunk scans data for sealed tokens and replaces them.
// Returns (output, remainder). If flush is true, all data is processed
// (no remainder held back).
func processChunk(data []byte, tokens map[string]string, flush bool) ([]byte, []byte) {
	if len(data) == 0 {
		return nil, nil
	}

	prefixBytes := []byte(tokenPrefix)
	var out bytes.Buffer
	i := 0

	for i < len(data) {
		// Search for token prefix from current position
		idx := bytes.Index(data[i:], prefixBytes)
		if idx < 0 {
			// No prefix found in remaining data
			if flush {
				out.Write(data[i:])
				return out.Bytes(), nil
			}
			// Hold back bytes that could be the start of a partial prefix
			safeEnd := findSafeFlushPoint(data[i:])
			out.Write(data[i : i+safeEnd])
			if i+safeEnd < len(data) {
				return out.Bytes(), copyBytes(data[i+safeEnd:])
			}
			return out.Bytes(), nil
		}

		tokenStart := i + idx

		// Write everything before the prefix
		out.Write(data[i:tokenStart])

		tokenEnd := tokenStart + tokenLen
		if tokenEnd > len(data) {
			// Token extends past end of data
			if flush {
				// No more data — this is literal text, not a complete token
				out.Write(data[tokenStart:])
				return out.Bytes(), nil
			}
			// Hold back the partial token for next read
			return out.Bytes(), copyBytes(data[tokenStart:])
		}

		// We have the full token candidate
		candidate := string(data[tokenStart:tokenEnd])
		if real, ok := tokens[candidate]; ok {
			out.Write([]byte(real))
		} else {
			// Looks like a token but not in our map — pass through unchanged
			out.Write(data[tokenStart:tokenEnd])
		}
		i = tokenEnd
	}

	return out.Bytes(), nil
}

// findSafeFlushPoint returns the number of bytes from the start of data
// that can be safely flushed (no partial prefix match at the end).
func findSafeFlushPoint(data []byte) int {
	prefix := []byte(tokenPrefix)
	// Check if any suffix of data matches a prefix of "osb_sealed_"
	for k := 1; k < len(prefix) && k <= len(data); k++ {
		if bytes.Equal(data[len(data)-k:], prefix[:k]) {
			return len(data) - k
		}
	}
	return len(data)
}

func copyBytes(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
