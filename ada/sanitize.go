package ada

import (
	"fmt"
	"unicode/utf8"
)

// defaultMaxOutput is how many bytes of any single stream survive into the
// model's context. The runtime never trusts stdout size (§5.1).
const defaultMaxOutput = 4096

// truncate keeps the informative ends of a stream (error banner up top, exit
// status at the bottom) and drops the middle, bounding context cost (§5.1).
func truncate(raw []byte, maxLen int) string {
	if len(raw) <= maxLen {
		return string(raw)
	}
	half := maxLen / 2
	return fmt.Sprintf("%s\n...[TRUNCATED %d BYTES]...\n%s",
		raw[:half], len(raw)-maxLen, raw[len(raw)-half:])
}

// isText reports whether the first chunk of d looks like UTF-8 text. A NUL byte
// is treated as a near-certain binary signal (§5.2).
func isText(d []byte) bool {
	n := len(d)
	if n > 512 {
		n = 512
	}
	for _, b := range d[:n] {
		if b == 0x00 {
			return false
		}
	}
	return utf8.Valid(d[:n])
}

// binaryHint replaces raw binary output before it can corrupt JSON or make the
// model hallucinate. The hint doubles as a curriculum: it teaches the model to
// reach for strings/xxd next time (§5.2).
const binaryHint = "[BINARY OUTPUT SUPPRESSED. Inspect with 'xxd | head' or 'strings' if needed.]"

// sanitizeOutput is the single chokepoint every byte passes through before it
// touches JSON or the model: binary detection first, then truncation (§5).
func sanitizeOutput(raw []byte, maxLen int) string {
	if !isText(raw) {
		return binaryHint
	}
	return truncate(raw, maxLen)
}
