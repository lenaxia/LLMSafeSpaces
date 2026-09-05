package sessionstate_test

import (
	"sync"
	"testing"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"

	sessionstate "github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
)

// r9: concurrent Metrics scrapes vs panic-ingesting — the -race pin for
// the atomic counter pair (write .Add under no lock, read .Load).
func TestMetrics_ConcurrentScrapeVsPanicIngest(t *testing.T) {
	auth, err := sessionstate.New(sessionstate.Config{Parser: &panicParserExported{}, Passwords: []string{"pw"}, PlatformDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			auth.Ingest([]byte(`{"type":"x"}`))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = auth.Metrics()
		}
	}()
	wg.Wait() // -race fires here if any read/write pair is unsynchronized
}

type panicParserExported struct{}

func (panicParserExported) Parse(_ []byte) (*abiv1.Event, bool, error) { panic("parser boom") }
