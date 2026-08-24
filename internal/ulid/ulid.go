// Package ulid ports src/ulid.ts: Crockford base32 ULIDs, monotonic within one
// Client. Sortable ids keep memory files listable in creation order without
// reading record bodies, and they are the provenance anchor that survives cold
// archival (architecture-v2.md §2).
//
// The generator owns mutable state (the monotonic counter), so it follows the
// Client/client pair of code-standards §1: state lives in an injected
// instance, never in a package variable, and the clock and entropy source are
// injected so tests are deterministic (§1.1). Wire exactly one Client per
// process to keep the monotonic guarantee process-wide.
package ulid

import (
	"crypto/rand"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/drakejin/memory-mcp/internal/errs"
)

// Ops carried by this package's errors (code-standards §2.1).
const (
	opNew            = "ulid.New"
	opValidateConfig = "ulid.Config.Validate"
	opGenerateAt     = "ulid.GenerateAt"
)

// entityULID is the entity these errors address.
const entityULID = "ulid"

// alphabet is Crockford base32: the digits plus the uppercase letters with
// I, L, O and U removed so ids cannot be misread.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	// timeLen / randomLen are the ULID field widths in encoded characters.
	timeLen   = 10
	randomLen = 16
	// encodedLen is the total id length; Valid rejects anything else.
	encodedLen = timeLen + randomLen
	// radix is the base of the alphabet.
	radix = 32
)

// Clock is the narrow time source the generator consumes. hotstore's clock
// satisfies it structurally (code-standards §1.1).
type Clock interface {
	Now() time.Time
}

// Client mints ULIDs. Ids minted by one Client are strictly increasing, so a
// process that shares a single Client can sort records by id alone.
type Client interface {
	// Generate returns a ULID for the clock's current time.
	Generate() (string, error)
	// GenerateAt returns a ULID for the given Unix-millisecond timestamp,
	// preserving monotonicity within the millisecond.
	GenerateAt(unixMillis int64) (string, error)
}

// Config is the single construction input.
type Config struct {
	// Clock supplies wall-clock time for Generate. Required.
	Clock Clock
	// Entropy is the randomness source for the id's random half. Nil uses
	// crypto/rand.Reader; tests inject a deterministic reader.
	Entropy io.Reader
}

// Validate reports whether the config can produce a usable generator.
func (c Config) Validate() error {
	if c.Clock == nil {
		return errs.Invalid(opValidateConfig, entityULID, "clock must be set")
	}
	return nil
}

// client is the concrete Client.
type client struct {
	clock   Clock
	entropy io.Reader

	// mu guards lastMillis and lastRandom. Invariant: two ids minted by this
	// client in the same millisecond never collide and always sort in mint
	// order, because the second one increments the first one's random half.
	mu         sync.Mutex
	lastMillis int64
	lastRandom []byte
}

// Compile-time contract check.
var _ Client = (*client)(nil)

// New returns a Client bound to cfg. It performs no I/O.
func New(cfg Config) (Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, errs.Wrap(opNew, err)
	}
	entropy := cfg.Entropy
	if entropy == nil {
		entropy = rand.Reader
	}
	return &client{clock: cfg.Clock, entropy: entropy}, nil
}

// Generate implements Client.
func (c *client) Generate() (string, error) {
	return c.GenerateAt(c.clock.Now().UnixMilli())
}

// GenerateAt implements Client.
func (c *client) GenerateAt(unixMillis int64) (string, error) {
	if unixMillis < 0 {
		return "", errs.Invalid(opGenerateAt, entityULID, "timestamp must not predate the unix epoch")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if unixMillis == c.lastMillis && len(c.lastRandom) == randomLen {
		// Same millisecond: bump the random part to keep strict ordering.
		// (The length guard fixes a latent src/ulid.ts edge where the very
		// first call at t=0 incremented an empty random part.)
		c.lastRandom = incrementRandom(c.lastRandom)
	} else {
		digits, err := c.randomPart()
		if err != nil {
			return "", errs.Internal(opGenerateAt, err)
		}
		c.lastMillis = unixMillis
		c.lastRandom = digits
	}

	var b strings.Builder
	b.Grow(encodedLen)
	b.WriteString(encodeTime(unixMillis))
	for _, d := range c.lastRandom {
		b.WriteByte(alphabet[d])
	}
	return b.String(), nil
}

// Valid reports whether value is a well-formed 26-character Crockford base32
// ULID. It is pure, so it needs no Client.
func Valid(value string) bool {
	if len(value) != encodedLen {
		return false
	}
	for i := range len(value) {
		if strings.IndexByte(alphabet, value[i]) < 0 {
			return false
		}
	}
	return true
}

// encodeTime renders the millisecond timestamp as the id's 10-character head.
func encodeTime(t int64) string {
	out := make([]byte, timeLen)
	for i := timeLen - 1; i >= 0; i-- {
		out[i] = alphabet[t%radix]
		t /= radix
	}
	return string(out)
}

// randomPart draws randomLen alphabet indexes from the entropy source.
func (c *client) randomPart() ([]byte, error) {
	buf := make([]byte, randomLen)
	if _, err := io.ReadFull(c.entropy, buf); err != nil {
		return nil, err
	}
	digits := make([]byte, randomLen)
	for i, b := range buf {
		digits[i] = b % radix
	}
	return digits, nil
}

// incrementRandom returns digits+1 in base 32, without mutating the input.
func incrementRandom(digits []byte) []byte {
	next := make([]byte, len(digits))
	copy(next, digits)
	for i := len(next) - 1; i >= 0; i-- {
		if next[i]+1 < radix {
			next[i]++
			return next
		}
		next[i] = 0
	}
	return next // overflow wraps; practically unreachable
}
