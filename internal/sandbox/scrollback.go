package sandbox

import "sync"

// OutputChunk is a tagged output chunk with a stream identifier.
type OutputChunk struct {
	Stream uint8  // 1=stdout, 2=stderr
	Data   []byte
	Seq    uint64
}

// ScrollbackBuffer is a thread-safe ring buffer for exec session output.
// It stores tagged output chunks up to a maximum total byte size and
// supports live subscribers for fan-out.
type ScrollbackBuffer struct {
	mu       sync.Mutex
	chunks   []OutputChunk
	totalLen int
	maxBytes int
	nextSeq  uint64
	subs     map[chan OutputChunk]struct{}
}

// NewScrollbackBuffer creates a new scrollback buffer with the given max size.
// If maxBytes is 0, defaults to 1MB.
func NewScrollbackBuffer(maxBytes int) *ScrollbackBuffer {
	if maxBytes <= 0 {
		maxBytes = 1024 * 1024 // 1MB
	}
	return &ScrollbackBuffer{
		maxBytes: maxBytes,
		subs:     make(map[chan OutputChunk]struct{}),
	}
}

// Write appends a chunk to the buffer and fans out to all subscribers.
func (sb *ScrollbackBuffer) Write(stream uint8, data []byte) {
	if len(data) == 0 {
		return
	}

	copied := make([]byte, len(data))
	copy(copied, data)

	sb.mu.Lock()
	chunk := OutputChunk{
		Stream: stream,
		Data:   copied,
		Seq:    sb.nextSeq,
	}
	sb.nextSeq++
	sb.chunks = append(sb.chunks, chunk)
	sb.totalLen += len(copied)

	// Evict oldest chunks if over limit
	for sb.totalLen > sb.maxBytes && len(sb.chunks) > 0 {
		sb.totalLen -= len(sb.chunks[0].Data)
		sb.chunks = sb.chunks[1:]
	}

	// Copy subscribers to avoid holding lock during send
	subs := make([]chan OutputChunk, 0, len(sb.subs))
	for ch := range sb.subs {
		subs = append(subs, ch)
	}
	sb.mu.Unlock()

	// Fan out to subscribers (non-blocking)
	for _, ch := range subs {
		select {
		case ch <- chunk:
		default:
			// Subscriber is slow, drop the chunk
		}
	}
}

// Snapshot returns all buffered chunks for replay.
func (sb *ScrollbackBuffer) Snapshot() []OutputChunk {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	result := make([]OutputChunk, len(sb.chunks))
	copy(result, sb.chunks)
	return result
}

// Subscribe returns a channel that receives live output chunks.
func (sb *ScrollbackBuffer) Subscribe() chan OutputChunk {
	ch := make(chan OutputChunk, 256)
	sb.mu.Lock()
	sb.subs[ch] = struct{}{}
	sb.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (sb *ScrollbackBuffer) Unsubscribe(ch chan OutputChunk) {
	sb.mu.Lock()
	delete(sb.subs, ch)
	sb.mu.Unlock()
}
