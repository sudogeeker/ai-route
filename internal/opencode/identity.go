package opencode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const identifierChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

type requestIdentity struct {
	session string
}

// clientProjectID renders the bare 40-character lowercase hex project value
// the OpenCode 2.x CLI sends. Upstream checks the shape, not the value.
func clientProjectID(session string) string {
	sum := sha256.Sum256([]byte("opencode-project\x00" + session))
	return hex.EncodeToString(sum[:20])
}

type identifierGenerator struct {
	mu        sync.Mutex
	timestamp int64
	counter   uint64
}

var identifiers identifierGenerator

func newRequestIdentity() (requestIdentity, error) {
	session, err := identifiers.next("ses", true, time.Now().UnixMilli())
	return requestIdentity{session: session}, err
}

// next matches the OpenCode client's six-byte time/counter encoding and random
// suffix. The mask is essential: formatting width alone does not truncate a
// uint64.
func (g *identifierGenerator) next(prefix string, descending bool, timestamp int64) (string, error) {
	var suffix [14]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	g.mu.Lock()
	if timestamp != g.timestamp {
		g.timestamp, g.counter = timestamp, 0
	}
	g.counter++
	value := uint64(timestamp)*4096 + g.counter
	g.mu.Unlock()
	if descending {
		value = ^value
	}
	for i, b := range suffix {
		suffix[i] = identifierChars[int(b)%len(identifierChars)]
	}
	return fmt.Sprintf("%s_%012x%s", prefix, value&0xffffffffffff, suffix[:]), nil
}
