package subprocess

import (
	"bytes"
	"fmt"
	"sync"
	"unicode/utf8"
)

// StreamingLineTruncator implements an io.Writer that bounds accumulated output
// in memory to maxBytes, preserving an initial head partition and a rolling tail ring buffer
// aligned to complete newline boundaries with UTF-8 rune validation.
type StreamingLineTruncator struct {
	maxBytes     int
	markerBudget int
	headCap      int
	tailCap      int

	headBuf      bytes.Buffer
	tailRing     []byte
	tailHead     int // next write index in circular ring
	tailLen      int // valid bytes stored in ring (up to tailCap)

	totalBytes   int64
	omittedLines int
	mu           sync.Mutex
}

// NewStreamingLineTruncator creates a new StreamingLineTruncator bounded to maxBytes.
func NewStreamingLineTruncator(maxBytes int) *StreamingLineTruncator {
	if maxBytes < 256 {
		maxBytes = 256
	}

	markerBudget := 120
	contentBudget := maxBytes - markerBudget
	if contentBudget < 60 {
		contentBudget = 60
	}

	headCap := contentBudget / 3
	tailCap := contentBudget - headCap

	return &StreamingLineTruncator{
		maxBytes:     maxBytes,
		markerBudget: markerBudget,
		headCap:      headCap,
		tailCap:      tailCap,
		tailRing:     make([]byte, tailCap),
	}
}

// Write processes incoming byte chunks, directing the first headCap bytes into headBuf
// and subsequent bytes into the circular tailRing, tracking dropped lines.
func (t *StreamingLineTruncator) Write(p []byte) (n int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, b := range p {
		t.totalBytes++
		if t.headBuf.Len() < t.headCap {
			t.headBuf.WriteByte(b)
		} else {
			if t.tailLen == t.tailCap {
				overwritten := t.tailRing[t.tailHead]
				if overwritten == '\n' {
					t.omittedLines++
				}
			} else {
				t.tailLen++
			}
			t.tailRing[t.tailHead] = b
			t.tailHead = (t.tailHead + 1) % t.tailCap
		}
	}
	return len(p), nil
}

// String assembles the preserved head, omission marker, and preserved tail.
func (t *StreamingLineTruncator) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.totalBytes <= int64(t.maxBytes) {
		if t.tailLen == 0 {
			return t.headBuf.String()
		}
		tailBytes := t.unrollTail()
		return t.headBuf.String() + string(tailBytes)
	}

	h := t.headBuf.Bytes()
	lastNL := bytes.LastIndexByte(h, '\n')
	var headSlice []byte
	var discardedHead []byte
	if lastNL != -1 {
		headSlice = h[:lastNL+1]
		discardedHead = h[lastNL+1:]
	} else {
		headSlice = trimToValidRune(h)
		discardedHead = h[len(headSlice):]
	}

	tailBytes := t.unrollTail()
	firstNL := bytes.IndexByte(tailBytes, '\n')
	var tailSlice []byte
	var discardedTail []byte
	if firstNL != -1 && firstNL+1 < len(tailBytes) {
		tailSlice = tailBytes[firstNL+1:]
		discardedTail = tailBytes[:firstNL+1]
	} else {
		tailSlice = alignToFirstRune(tailBytes)
		discardedTail = tailBytes[:len(tailBytes)-len(tailSlice)]
	}

	omittedBytes := t.totalBytes - int64(len(headSlice)+len(tailSlice))
	discardedNL := bytes.Count(discardedHead, []byte{'\n'}) + bytes.Count(discardedTail, []byte{'\n'})
	totalOmittedLines := t.omittedLines + discardedNL

	marker := fmt.Sprintf("\n\n[... truncated: %d lines (%d bytes) omitted to protect context window ...]\n\n",
		totalOmittedLines, omittedBytes)

	res := string(headSlice) + marker + string(tailSlice)
	if len(res) > t.maxBytes {
		res = res[:t.maxBytes]
	}
	return res
}

// TotalBytes returns the total number of bytes written to the truncator.
func (t *StreamingLineTruncator) TotalBytes() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.totalBytes
}

// OmittedBytes returns the total number of bytes omitted in the assembled output.
func (t *StreamingLineTruncator) OmittedBytes() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.totalBytes <= int64(t.maxBytes) {
		return 0
	}
	hLen, tLen := t.projectedContentLengths()
	return t.totalBytes - int64(hLen+tLen)
}

// OmittedLines returns the total count of newline characters dropped.
func (t *StreamingLineTruncator) OmittedLines() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.totalBytes <= int64(t.maxBytes) {
		return 0
	}
	h := t.headBuf.Bytes()
	lastNL := bytes.LastIndexByte(h, '\n')
	var discardedHead []byte
	if lastNL != -1 {
		discardedHead = h[lastNL+1:]
	} else {
		headSlice := trimToValidRune(h)
		discardedHead = h[len(headSlice):]
	}

	tailBytes := t.unrollTail()
	firstNL := bytes.IndexByte(tailBytes, '\n')
	var discardedTail []byte
	if firstNL != -1 && firstNL+1 < len(tailBytes) {
		discardedTail = tailBytes[:firstNL+1]
	} else {
		tailSlice := alignToFirstRune(tailBytes)
		discardedTail = tailBytes[:len(tailBytes)-len(tailSlice)]
	}

	return t.omittedLines + bytes.Count(discardedHead, []byte{'\n'}) + bytes.Count(discardedTail, []byte{'\n'})
}

func (t *StreamingLineTruncator) unrollTail() []byte {
	out := make([]byte, t.tailLen)
	start := (t.tailHead - t.tailLen + t.tailCap) % t.tailCap
	for i := 0; i < t.tailLen; i++ {
		out[i] = t.tailRing[(start+i)%t.tailCap]
	}
	return out
}

func (t *StreamingLineTruncator) projectedContentLengths() (int, int) {
	h := t.headBuf.Bytes()
	lastNL := bytes.LastIndexByte(h, '\n')
	hLen := 0
	if lastNL != -1 {
		hLen = lastNL + 1
	} else {
		hLen = len(trimToValidRune(h))
	}

	tailBytes := t.unrollTail()
	firstNL := bytes.IndexByte(tailBytes, '\n')
	tLen := 0
	if firstNL != -1 && firstNL+1 < len(tailBytes) {
		tLen = len(tailBytes[firstNL+1:])
	} else {
		tLen = len(alignToFirstRune(tailBytes))
	}
	return hLen, tLen
}

func trimToValidRune(b []byte) []byte {
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size == 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

func alignToFirstRune(b []byte) []byte {
	for len(b) > 0 {
		if utf8.RuneStart(b[0]) {
			r, _ := utf8.DecodeRune(b)
			if r != utf8.RuneError {
				return b
			}
		}
		b = b[1:]
	}
	return b
}
