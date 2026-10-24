package opencode

import (
	"regexp"
	"strings"
	"sync"
	"testing"
)

var sessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var messagePattern = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func TestIdentifierEncoding(t *testing.T) {
	var generator identifierGenerator
	// Timestamp 2^36 + 1 exercises truncation to the low six bytes.
	const timestamp = 1<<36 + 1
	for _, test := range []struct {
		prefix     string
		descending bool
		want       string
		pattern    *regexp.Regexp
	}{
		{"ses", true, "ses_ffffffffeffe", sessionPattern},
		{"msg", false, "msg_000000001002", messagePattern},
		{"msg", false, "msg_000000001003", messagePattern},
	} {
		id, err := generator.next(test.prefix, test.descending, timestamp)
		if err != nil || !test.pattern.MatchString(id) || !strings.HasPrefix(id, test.want) {
			t.Fatalf("unexpected identifier %q, wanted prefix %q: %v", id, test.want, err)
		}
	}
	id, err := generator.next("msg", false, timestamp+1)
	if err != nil || !strings.HasPrefix(id, "msg_000000002001") {
		t.Fatalf("counter did not reset on next millisecond: %q %v", id, err)
	}
}

func TestIdentifiersConcurrent(t *testing.T) {
	var generator identifierGenerator
	const count = 1000
	ids := make(chan string, count)
	var workers sync.WaitGroup
	for i := 0; i < count; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			id, err := generator.next("msg", false, 1<<36+1)
			if err != nil {
				t.Errorf("generate identifier: %v", err)
				return
			}
			ids <- id
		}()
	}
	workers.Wait()
	close(ids)
	seen := make(map[string]bool)
	for id := range ids {
		// Distinct encoded counters are required even if random suffixes differ.
		prefix := id[:16]
		if !messagePattern.MatchString(id) || seen[prefix] {
			t.Fatalf("invalid or reused identifier counter: %q", id)
		}
		seen[prefix] = true
	}
	if len(seen) != count {
		t.Fatalf("generated %d identifiers, expected %d", len(seen), count)
	}
}

func TestRequestIdentityUsesGlobalProject(t *testing.T) {
	first, err := newRequestIdentity()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newRequestIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if first.project != "global" || second.project != "global" {
		t.Fatalf("project IDs do not match the CLI: %q %q", first.project, second.project)
	}
}
