// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package grpc

import "sync"

// detachableRingBufSize is the scrollback capacity per detachable PTY session.
const detachableRingBufSize = 256 * 1024 // 256 KiB

// RingBuffer is a thread-safe fixed-capacity circular byte buffer.
// When full, new writes overwrite the oldest data (like a terminal scrollback).
type RingBuffer struct {
	mu       sync.Mutex
	buf      []byte
	cap      int
	head     int // index of the oldest byte
	used     int // number of valid bytes currently stored
	writePos int // monotonic count of bytes ever written
	markPos  int // writePos value at the last Mark() call; 0 = no mark
}

func newRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{buf: make([]byte, capacity), cap: capacity}
}

// Write appends data to the buffer, overwriting the oldest bytes when full.
func (r *RingBuffer) Write(data []byte) {
	if len(data) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.writePos += len(data)

	if len(data) >= r.cap {
		copy(r.buf, data[len(data)-r.cap:])
		r.head = 0
		r.used = r.cap
		return
	}

	for _, b := range data {
		idx := (r.head + r.used) % r.cap
		if r.used == r.cap {
			r.buf[idx] = b
			r.head = (r.head + 1) % r.cap
		} else {
			r.buf[idx] = b
			r.used++
		}
	}
}

// Snapshot returns a copy of all buffered data in order (oldest to newest).
func (r *RingBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.used == 0 {
		return nil
	}
	out := make([]byte, r.used)
	if r.head+r.used <= r.cap {
		copy(out, r.buf[r.head:r.head+r.used])
	} else {
		n := copy(out, r.buf[r.head:])
		copy(out[n:], r.buf[:r.used-n])
	}
	return out
}

// Mark records the current write position.  A subsequent SnapshotSinceMark
// call will return only bytes written after this point.
func (r *RingBuffer) Mark() {
	r.mu.Lock()
	r.markPos = r.writePos
	r.mu.Unlock()
}

// SnapshotSinceMark returns only the bytes written since the last Mark call.
// If no mark has been set, it behaves like Snapshot.
// If more bytes were written than the buffer capacity since the mark, all
// currently buffered bytes are returned (the oldest ones were overwritten).
func (r *RingBuffer) SnapshotSinceMark() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.used == 0 {
		return nil
	}

	bytesAfterMark := r.writePos - r.markPos
	if bytesAfterMark <= 0 {
		return nil
	}
	if bytesAfterMark > r.used {
		bytesAfterMark = r.used
	}

	skip := r.used - bytesAfterMark
	out := make([]byte, bytesAfterMark)
	startIdx := (r.head + skip) % r.cap
	if startIdx+bytesAfterMark <= r.cap {
		copy(out, r.buf[startIdx:startIdx+bytesAfterMark])
	} else {
		n := copy(out, r.buf[startIdx:])
		copy(out[n:], r.buf[:bytesAfterMark-n])
	}
	return out
}
