// Package ulid ports src/ulid.ts: Crockford base32 ULIDs, monotonic within the
// process. Sortable ids keep memory files listable in creation order without
// reading record bodies. Fully implemented by the scaffold.
package ulid

import (
	"crypto/rand"
	"regexp"
	"strings"
	"sync"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	timeLen   = 10
	randomLen = 16
)

var pattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

var (
	mu         sync.Mutex
	lastTime   int64
	lastRandom []byte
)

// New returns a ULID for the current wall-clock time. Successive calls within
// the same millisecond increment the random part so ids stay strictly ordered.
func New() string {
	return At(time.Now().UnixMilli())
}

// At returns a ULID for the given Unix-millisecond timestamp, preserving
// process-wide monotonicity (same contract as ulid(now) in src/ulid.ts).
func At(nowMillis int64) string {
	mu.Lock()
	defer mu.Unlock()
	if nowMillis == lastTime && len(lastRandom) == randomLen {
		// Same millisecond: bump the random part to keep strict ordering.
		// (The len guard fixes a latent src/ulid.ts edge where the very first
		// call at t=0 incremented an empty random part.)
		lastRandom = incrementRandom(lastRandom)
	} else {
		lastTime = nowMillis
		lastRandom = randomPart()
	}
	var b strings.Builder
	b.Grow(timeLen + randomLen)
	b.WriteString(encodeTime(nowMillis))
	for _, d := range lastRandom {
		b.WriteByte(alphabet[d])
	}
	return b.String()
}

// IsULID reports whether value is a well-formed 26-char Crockford-base32 ULID.
func IsULID(value string) bool {
	return pattern.MatchString(value)
}

func encodeTime(t int64) string {
	out := make([]byte, timeLen)
	for i := timeLen - 1; i >= 0; i-- {
		out[i] = alphabet[t%32]
		t /= 32
	}
	return string(out)
}

func randomPart() []byte {
	bytes := make([]byte, randomLen)
	if _, err := rand.Read(bytes); err != nil {
		// crypto/rand never fails on supported platforms; keep the port total.
		panic("ulid: crypto/rand unavailable: " + err.Error())
	}
	digits := make([]byte, randomLen)
	for i, b := range bytes {
		digits[i] = b % 32
	}
	return digits
}

func incrementRandom(digits []byte) []byte {
	next := make([]byte, len(digits))
	copy(next, digits)
	for i := len(next) - 1; i >= 0; i-- {
		if next[i]+1 < 32 {
			next[i]++
			return next
		}
		next[i] = 0
	}
	return next // overflow wraps; practically unreachable
}
