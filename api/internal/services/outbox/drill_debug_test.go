package outbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDrillDebug(t *testing.T) {
	s, mr := newTestService(t)
	defer mr.Close()
	ctx := context.Background()
	d := &recordingDeliverer{delay: 2 * time.Millisecond}
	runCtx, stopRun := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(runCtx, d.deliver, 5*time.Millisecond); close(done) }()

	for i := 1; i <= 40; i++ {
		_, err := s.Accept(ctx, "ws-drill", fmt.Sprintf("ses-%d", i%4), "u1", fmt.Sprintf("cm-%d", i), "drill", nil)
		require.NoError(t, err)
	}
	time.Sleep(30 * time.Millisecond)
	stopRun()
	<-done
	parked, err := s.ParkWorkspace(ctx, "ws-drill", "debug")
	require.NoError(t, err)
	unparked, err := s.UnparkWorkspace(ctx, "ws-drill")
	require.NoError(t, err)
	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()
	go func() { s.Run(drainCtx, d.deliver, 5*time.Millisecond) }()
	time.Sleep(2 * time.Second)
	got, dupes := d.counts()
	t.Logf("parked=%d unparked=%d delivered=%d dupes=%d", parked, unparked, got, dupes)
	for ses := 0; ses < 4; ses++ {
		q := readQueueEntries(t, s, "ws-drill", fmt.Sprintf("ses-%d", ses))
		staged := s.client.LRange(context.Background(), dKey("ws-drill", fmt.Sprintf("ses-%d", ses)), 0, -1).Val()
		for _, e := range q {
			t.Logf("  ses-%d queue: %s status=%s next=%v", ses, e.ID, e.Status, e.NextAttemptAt)
		}
		if len(staged) > 0 {
			t.Logf("  ses-%d staged: %d", ses, len(staged))
		}
	}
}
