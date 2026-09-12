package inbox

import (
	"context"
	"fmt"
	"testing"
)

func TestProbeResolveKeepsField(t *testing.T) {
	svc, mr := newTestService(t)
	ctx := context.Background()
	if err := svc.Record(ctx, "ws1", questionRecord("que_x")); err != nil {
		t.Fatal(err)
	}
	raw := mr.HGet("inboxq:ws1:ses_a", "que_x")
	fmt.Printf("BEFORE raw=%.80s\n", raw)
	if err := svc.Resolve(ctx, "ws1", "ses_a", "que_x", StatusAnswered); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	raw2 := mr.HGet("inboxq:ws1:ses_a", "que_x")
	fmt.Printf("AFTER raw=%.140s\n", raw2)
}
