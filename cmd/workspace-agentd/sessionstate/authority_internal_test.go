package sessionstate

import (
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// r9: the flush-path containment pin + the concurrent-scrape race pin.
// The r8 deadlock: a lock inside parseContained's recover deadlocked the
// reseed flush (which parses UNDER a.mu). The r9 race: an atomic write
// against a plain read under -race. Both must stay pinned.
func TestParseContained_PanicUnderLockNoDeadlock(t *testing.T) {
	auth, err := New(Config{Parser: &panicParser{}, Passwords: []string{"pw"}, PlatformDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		auth.mu.Lock()
		// The exact flush-path shape: parse while HOLDING the lock.
		_, _, _ = auth.parseContained([]byte(`{"type":"anything"}`))
		auth.mu.Unlock()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("parseContained under a.mu deadlocked — the flush-path regression")
	}
	if got := auth.panicsContained.Load(); got != 1 {
		t.Fatalf("panicsContained = %d, want 1", got)
	}
}

type panicParser struct{}

func (panicParser) Parse(_ []byte) (*abiv1.Event, bool, error) { panic("parser boom") }
