package agent

import "sync"

// RingBuffer is a bounded, append-only byte stream with a monotonically
// increasing logical offset, per base spec §10.2-§10.3. Once capacity is
// exceeded, the oldest bytes are dropped; ReadFrom reports
// truncatedBefore when the caller's requested offset has already been
// dropped.
type RingBuffer struct {
	mu    sync.Mutex
	buf   []byte // logical bytes currently retained, oldest first
	cap   int
	start int64 // logical offset of buf[0]
}

func NewRingBuffer(capBytes int) *RingBuffer {
	return &RingBuffer{cap: capBytes}
}

func (r *RingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.cap; over > 0 {
		r.buf = r.buf[over:]
		r.start += int64(over)
	}
	return len(p), nil
}

// Len returns the total number of bytes ever written (the end offset).
func (r *RingBuffer) Len() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.start + int64(len(r.buf))
}

func (r *RingBuffer) ReadFrom(offset int64, maxBytes int) (data []byte, nextOffset int64, truncatedBefore bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if offset < r.start {
		truncatedBefore = true
		offset = r.start
	}
	relStart := offset - r.start
	if relStart >= int64(len(r.buf)) {
		return nil, offset, truncatedBefore
	}
	end := int64(len(r.buf))
	if maxBytes > 0 && relStart+int64(maxBytes) < end {
		end = relStart + int64(maxBytes)
	}
	out := make([]byte, end-relStart)
	copy(out, r.buf[relStart:end])
	return out, r.start + end, truncatedBefore
}
