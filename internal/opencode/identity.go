package opencode

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

const identifierChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

type requestIdentity struct {
	session string
	request string
}

// openCodeProjectID is the stateless CLI project fingerprint. The captured
// anonymous request uses the global project rather than hashing the session.
const openCodeProjectID = "global"

type identifierGenerator struct {
	mu        sync.Mutex
	timestamp int64
	counter   uint64
}

var identifiers identifierGenerator

func newRequestIdentity() (requestIdentity, error) {
	now := time.Now().UnixMilli()
	session, err := identifiers.next("ses", true, now)
	if err != nil {
		return requestIdentity{}, err
	}
	request, err := identifiers.next("msg", false, now)
	return requestIdentity{session: session, request: request}, err
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
