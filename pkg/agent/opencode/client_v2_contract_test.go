package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// Contract goldens — captured from live opencode 1.18.15 on 2026-08-28
// (#1119). These encode the wire shapes the adapter depends on so that the
// NEXT upstream patch-release drift fails here, in CI, instead of in
// production (the {modelID} → {id} Model.Ref change shipped silently and
// broke per-prompt model overrides; the probe + these goldens are the
// tripwire).

// goldenProbeMissingKeyID is the 400 body a 1.18.15 binary returns for
// POST /api/session/:sid/model with {"model":{}} (captured live).
const goldenProbeMissingKeyID = `{"_tag":"InvalidRequestError","message":"Missing key\n  at [\"model\"][\"id\"]","kind":"Payload"}`

// goldenProbeMissingKeyModelID is the equivalent for the <= 1.18.14 shape.
const goldenProbeMissingKeyModelID = `{"_tag":"InvalidRequestError","message":"Missing key\n  at [\"model\"][\"modelID\"]","kind":"Payload"}`

// goldenAdmittedEvent / goldenPromptedEvent are captured from the durable
// per-session event log (real capture, stress workspace ses_fb70a654,
// 2026-08-28). NOTE the load-bearing detail: prompted carries the USER
// messageID — the same id the admission returns — which is the correlation
// key for the #1119 delivery-acknowledgment seam.
const goldenAdmittedEvent = `{"id":"evt_048f59c17002smP7EYvhLnuk2LkZ","type":"session.next.prompt.admitted","durable":{"aggregateID":"ses_fb70a6547ffefMTRGKDBfJs2RD","seq":1,"version":1},"data":{"timestamp":1787930450967,"sessionID":"ses_fb70a6547ffefMTRGKDBfJs2RD","messageID":"msg_048f59c170013F1qt8QPxNVLaS","prompt":{"text":"reply with exactly: NODELIVERY_TEST_1"},"delivery":"steer"}}`

const goldenPromptedEvent = `{"id":"evt_048f59daf001wOFaOFX6V9lhPr","type":"session.next.prompted","durable":{"aggregateID":"ses_fb70a6547ffefMTRGKDBfJs2RD","seq":2,"version":1},"data":{"timestamp":1787930450967,"sessionID":"ses_fb70a6547ffefMTRGKDBfJs2RD","messageID":"msg_048f59c170013F1qt8QPxNVLaS","prompt":{"text":"reply with exactly: NODELIVERY_TEST_1"},"delivery":"steer"}}`

func newContractClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "testpw", zap.NewNop()), srv
}

func TestCapabilityProbeModelRefShapes(t *testing.T) {
	cases := []struct {
		name       string
		modelBody  string
		wantIDKey  bool
		wantProbed bool
	}{
		{"1.18.15 id-key shape", goldenProbeMissingKeyID, true, true},
		{"1.18.14 modelID-key shape", goldenProbeMissingKeyModelID, false, true},
		{"404 first (existence pre-check) — indeterminate", `{"error":"not found"}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newContractClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/model"):
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(tc.modelBody))
				case strings.HasSuffix(r.URL.Path, "/prompt"):
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"_tag":"InvalidRequestError","message":"Missing key"}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			caps, err := c.Capabilities(context.Background())
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if !caps.Probed {
				t.Fatal("Probed must be true after probe")
			}
			if !caps.V2PromptRoute {
				t.Fatal("V2PromptRoute should be detected via 400 InvalidRequestError")
			}
			if caps.ModelRefIDKey != tc.wantIDKey {
				t.Fatalf("ModelRefIDKey = %v, want %v", caps.ModelRefIDKey, tc.wantIDKey)
			}
		})
	}
}

func TestPromptV2WireShapePerCapability(t *testing.T) {
	cases := []struct {
		name      string
		modelBody string
		wantModel string // exact JSON of the model field
	}{
		// The 1.18.15 golden: id-key object. This is the shape a live
		// binary accepted with 204 on POST /api/session/:sid/model and the
		// one per-prompt overrides MUST use on the pinned floor runtime.
		{"id-key on 1.18.15", goldenProbeMissingKeyID, `{"id":"glm-5.3","providerID":"thekaocloud"}`},
		{"modelID-key on legacy", goldenProbeMissingKeyModelID, `{"modelID":"glm-5.3","providerID":"thekaocloud"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody string
			c, _ := newContractClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/model"):
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(tc.modelBody))
				case strings.HasSuffix(r.URL.Path, "/prompt"):
					b, _ := io.ReadAll(r.Body)
					gotBody = string(b)
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"data":{"id":"msg_1","delivery":"queue"}}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			_, err := c.PromptV2WithModel(context.Background(), "ses_1", "hi", V2DeliveryQueue,
				&V2ModelRef{ModelID: "glm-5.3", ProviderID: "thekaocloud"})
			if err != nil {
				t.Fatalf("PromptV2WithModel: %v", err)
			}
			var decoded struct {
				Prompt struct {
					Text  string          `json:"text"`
					Model json.RawMessage `json:"model"`
				} `json:"prompt"`
				Delivery string `json:"delivery"`
			}
			if err := json.Unmarshal([]byte(gotBody), &decoded); err != nil {
				t.Fatalf("request body not valid JSON: %v\n%s", err, gotBody)
			}
			if string(decoded.Prompt.Model) != tc.wantModel {
				t.Fatalf("model ref wire shape = %s, want %s", decoded.Prompt.Model, tc.wantModel)
			}
			if decoded.Delivery != "queue" {
				t.Fatalf("delivery = %s, want queue", decoded.Delivery)
			}
		})
	}
}

func TestPromptV2NilModelOmitsField(t *testing.T) {
	var gotBody string
	c, _ := newContractClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"id":"msg_1","delivery":"queue"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := c.PromptV2(context.Background(), "ses_1", "hi", V2DeliveryQueue); err != nil {
		t.Fatalf("PromptV2: %v", err)
	}
	if strings.Contains(gotBody, "model") {
		t.Fatalf("nil override must omit the model field entirely, got %s", gotBody)
	}
}

func TestGoldenEventShapesDecode(t *testing.T) {
	// The delivery acknowledgment seam (#1119 fix design) correlates
	// messageIDs across these two events. If upstream renames or reshapes
	// them, this fails before the outbox code ever sees the binary.
	for name, raw := range map[string]string{
		"admitted": goldenAdmittedEvent,
		"prompted": goldenPromptedEvent,
	} {
		t.Run(name, func(t *testing.T) {
			var ev struct {
				Type    string `json:"type"`
				Durable struct {
					AggregateID string `json:"aggregateID"`
					Seq         int64  `json:"seq"`
					Version     int    `json:"version"`
				} `json:"durable"`
				Data struct {
					SessionID          string `json:"sessionID"`
					MessageID          string `json:"messageID"`
					AssistantMessageID string `json:"assistantMessageID"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				t.Fatalf("decode golden %s: %v", name, err)
			}
			if ev.Durable.Seq == 0 || ev.Durable.AggregateID == "" {
				t.Fatalf("golden %s lost durable envelope: %+v", name, ev.Durable)
			}
			if ev.Data.SessionID == "" {
				t.Fatalf("golden %s lost sessionID", name)
			}
		})
	}
}
