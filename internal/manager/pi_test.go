package manager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sklarsa/kanedias/internal/attachments"
	"github.com/sklarsa/kanedias/internal/supervisor"
)

// ---- Generic Pi response validation ----

func TestDecodePiResponseRejectsInvalidJSON(t *testing.T) {
	_, err := decodePiResponse[any](json.RawMessage(`not-json`), "cmd")
	if err == nil {
		t.Fatal("accepted invalid JSON")
	}
}

func TestDecodePiResponseRejectsSuccessFalse(t *testing.T) {
	raw := json.RawMessage(`{"type":"response","command":"cmd","success":false,"error":"oops"}`)
	_, err := decodePiResponse[any](raw, "cmd")
	if err == nil {
		t.Fatal("accepted success:false")
	}
}

func TestDecodePiResponseRejectsMissingType(t *testing.T) {
	raw := json.RawMessage(`{"command":"cmd","success":true}`)
	_, err := decodePiResponse[any](raw, "cmd")
	if err == nil {
		t.Fatal("accepted missing type field")
	}
}

func TestDecodePiResponseAcceptsValid(t *testing.T) {
	raw := json.RawMessage(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":false}}`)
	data, err := decodePiResponse[piStateData](raw, "get_state")
	if err != nil {
		t.Fatalf("rejected valid response: %v", err)
	}
	if data.IsStreaming {
		t.Fatal("expected isStreaming=false")
	}
}

func TestDecodePiResponseRejectsWrongCommand(t *testing.T) {
	raw := json.RawMessage(`{"type":"response","command":"other","success":true}`)
	_, err := decodePiResponse[any](raw, "expected")
	if err == nil {
		t.Fatal("accepted wrong command")
	}
}

func TestDecodePiResponseRejectsMissingExpectedCommand(t *testing.T) {
	raw := json.RawMessage(`{"type":"response","success":true}`)
	_, err := decodePiResponse[any](raw, "expected")
	if err == nil {
		t.Fatal("accepted missing expected command")
	}
}

// ---- Steer: streaming vs. idle dispatch ----

// piControlClient is a rootClient that records the exact sequence of CallRPC
// invocations and allows test-specific response injection.
type piControlClient struct {
	callLog  []piCall
	response func(idx int, sessionID string, payload json.RawMessage) (json.RawMessage, error)
}

type piCall struct {
	sessionID string
	payload   json.RawMessage
}

func (c *piControlClient) CallRPC(_ context.Context, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	idx := len(c.callLog)
	c.callLog = append(c.callLog, piCall{sessionID: sessionID, payload: append(json.RawMessage(nil), payload...)})
	if c.response != nil {
		return c.response(idx, sessionID, payload)
	}
	return json.RawMessage(`{"type":"response","command":"","success":true}`), nil
}

func (c *piControlClient) Snapshot(_ context.Context) (supervisor.NodeSnapshot, error) {
	return supervisor.NodeSnapshot{}, nil
}
func (c *piControlClient) Subscribe(_ context.Context) (supervisor.Subscription, error) {
	ch := make(chan supervisor.EventEnvelope)
	close(ch)
	return supervisor.Subscription{Events: ch, Close: func() {}}, nil
}
func (c *piControlClient) AnswerQuestion(_ context.Context, _, _ string, _ json.RawMessage) error {
	return nil
}
func (c *piControlClient) Stop(_ context.Context, _ string) error { return nil }
func (c *piControlClient) Close() error                           { return nil }

func piManagerWithSession(sessionID string, client *piControlClient, tree supervisor.NodeSnapshot) *Manager {
	m := fakeManager(nil)
	handle := &rootHandle{
		socketPath: "/tmp/" + sessionID + ".root.sock",
		rootID:     tree.SessionID,
		actionable: true,
		client:     client,
		tree:       tree,
		mirror:     newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
	}
	m.roots[handle.socketPath] = handle
	m.routes[sessionID] = handle.rootID
	return m
}

func imageCapablePIControlClient(streaming bool) *piControlClient {
	client := &piControlClient{}
	client.response = func(idx int, _ string, _ json.RawMessage) (json.RawMessage, error) {
		if idx == 0 {
			if streaming {
				return json.RawMessage(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":true,"model":{"input":["text","image"]}}}`), nil
			}
			return json.RawMessage(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":false,"model":{"input":["text","image"]}}}`), nil
		}
		if streaming {
			return json.RawMessage(`{"type":"response","command":"steer","success":true}`), nil
		}
		return json.RawMessage(`{"type":"response","command":"prompt","success":true}`), nil
	}
	return client
}

func testPNG(t *testing.T, marker byte) attachments.Image {
	t.Helper()
	data := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	data[len(data)-1] = marker
	image, err := attachments.NewImage(data)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func TestSendMessageIdleIncludesNativeImages(t *testing.T) {
	image := testPNG(t, 1)
	client := imageCapablePIControlClient(false)
	m := piManagerWithSession("root", client, rootTree("root"))

	if err := m.SendMessage(context.Background(), "root", "inspect", []attachments.Image{image}); err != nil {
		t.Fatal(err)
	}
	var command struct {
		Type, Message string
		Images        []struct{ Type, Data, MIMEType string }
	}
	if err := json.Unmarshal(client.callLog[1].payload, &command); err != nil {
		t.Fatal(err)
	}
	if command.Type != "prompt" || command.Message != "inspect" {
		t.Fatalf("command = %#v", command)
	}
	if len(command.Images) != 1 || command.Images[0].Type != "image" || command.Images[0].MIMEType != "image/png" {
		t.Fatalf("images = %#v", command.Images)
	}
	if got := command.Images[0].Data; got != base64.StdEncoding.EncodeToString(image.Data) {
		t.Fatal("data mismatch")
	}
}

func TestSendMessageStreamingIncludesNativeImages(t *testing.T) {
	first := testPNG(t, 1)
	second := testPNG(t, 2)
	client := imageCapablePIControlClient(true)
	m := piManagerWithSession("root", client, rootTree("root"))

	if err := m.SendMessage(context.Background(), "root", "redirect", []attachments.Image{first, second}); err != nil {
		t.Fatal(err)
	}
	var command struct {
		Type, Message string
		Images        []struct{ Type, Data, MIMEType string }
	}
	if err := json.Unmarshal(client.callLog[1].payload, &command); err != nil {
		t.Fatal(err)
	}
	if command.Type != "steer" || command.Message != "redirect" {
		t.Fatalf("command = %#v", command)
	}
	want := []struct{ Type, Data, MIMEType string }{
		{Type: "image", Data: base64.StdEncoding.EncodeToString(first.Data), MIMEType: "image/png"},
		{Type: "image", Data: base64.StdEncoding.EncodeToString(second.Data), MIMEType: "image/png"},
	}
	if len(command.Images) != len(want) {
		t.Fatalf("streaming images = %#v, want %#v", command.Images, want)
	}
	for index := range want {
		if command.Images[index] != want[index] {
			t.Fatalf("streaming image %d = %#v, want %#v", index, command.Images[index], want[index])
		}
	}
}

func TestSendMessageCanonicalizesJPEGAlias(t *testing.T) {
	data := append([]byte("\xff\xd8\xff\xe0JFIF\x00"), make([]byte, 32)...)
	image := attachments.Image{MIMEType: "image/jpg", Data: data}
	client := imageCapablePIControlClient(false)
	m := piManagerWithSession("root", client, rootTree("root"))

	if err := m.SendMessage(context.Background(), "root", "inspect", []attachments.Image{image}); err != nil {
		t.Fatal(err)
	}
	var command piMessageCommand
	if err := json.Unmarshal(client.callLog[1].payload, &command); err != nil {
		t.Fatal(err)
	}
	if got := command.Images[0].MIMEType; got != "image/jpeg" {
		t.Fatalf("MIMEType = %q, want image/jpeg", got)
	}
}

func TestSendMessagePreservesImageOrder(t *testing.T) {
	first := testPNG(t, 1)
	second := testPNG(t, 2)
	client := imageCapablePIControlClient(false)
	m := piManagerWithSession("root", client, rootTree("root"))

	if err := m.SendMessage(context.Background(), "root", "compare", []attachments.Image{first, second}); err != nil {
		t.Fatal(err)
	}
	var command struct {
		Images []struct{ Data string }
	}
	if err := json.Unmarshal(client.callLog[1].payload, &command); err != nil {
		t.Fatal(err)
	}
	want := []string{base64.StdEncoding.EncodeToString(first.Data), base64.StdEncoding.EncodeToString(second.Data)}
	if len(command.Images) != len(want) {
		t.Fatalf("got %d images, want %d", len(command.Images), len(want))
	}
	for i := range want {
		if command.Images[i].Data != want[i] {
			t.Fatalf("image %d data mismatch", i)
		}
	}
}

func TestSteerOmitsImagesField(t *testing.T) {
	client := imageCapablePIControlClient(false)
	m := piManagerWithSession("root", client, rootTree("root"))

	if err := m.Steer(context.Background(), "root", "text only"); err != nil {
		t.Fatal(err)
	}
	var command map[string]json.RawMessage
	if err := json.Unmarshal(client.callLog[1].payload, &command); err != nil {
		t.Fatal(err)
	}
	if _, found := command["images"]; found {
		t.Fatalf("Steer payload contains images: %s", client.callLog[1].payload)
	}
}

func TestSendMessageRejectsModelsWithoutImageInput(t *testing.T) {
	tests := []struct {
		name      string
		modelJSON string
	}{
		{"nil_model", ""},
		{"text_only", `,"model":{"input":["text"]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &piControlClient{response: func(_ int, _ string, _ json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":false` + test.modelJSON + `}}`), nil
			}}
			m := piManagerWithSession("root", client, rootTree("root"))
			err := m.SendMessage(context.Background(), "root", "inspect", []attachments.Image{testPNG(t, 1)})
			if !errors.Is(err, ErrImageInputUnsupported) {
				t.Fatalf("SendMessage() error = %v, want %v", err, ErrImageInputUnsupported)
			}
			if len(client.callLog) != 1 {
				t.Fatalf("RPC calls = %d, want 1", len(client.callLog))
			}
		})
	}
}

func TestSendMessageAcceptsTextAndImageModelInput(t *testing.T) {
	client := imageCapablePIControlClient(false)
	m := piManagerWithSession("root", client, rootTree("root"))
	if err := m.SendMessage(context.Background(), "root", "inspect", []attachments.Image{testPNG(t, 1)}); err != nil {
		t.Fatalf("SendMessage() error: %v", err)
	}
}

func TestSendMessageValidatesImagesBeforeNetworkIO(t *testing.T) {
	client := imageCapablePIControlClient(false)
	m := piManagerWithSession("root", client, rootTree("root"))
	valid := testPNG(t, 1)
	invalid := attachments.Image{MIMEType: "image/png", Data: []byte("not an image")}

	err := m.SendMessage(context.Background(), "root", "inspect", []attachments.Image{valid, invalid})
	if !errors.Is(err, attachments.ErrUnsupportedFormat) {
		t.Fatalf("SendMessage() error = %v, want %v", err, attachments.ErrUnsupportedFormat)
	}
	if len(client.callLog) != 0 {
		t.Fatalf("invalid images caused %d RPC calls", len(client.callLog))
	}
}

func TestSendMessageRejectsMissingPromptOrSteerCommand(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "prompt", true: "steer"}[streaming], func(t *testing.T) {
			client := imageCapablePIControlClient(streaming)
			client.response = func(idx int, _ string, _ json.RawMessage) (json.RawMessage, error) {
				if idx == 0 {
					streamingJSON := "false"
					if streaming {
						streamingJSON = "true"
					}
					return json.RawMessage(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":` + streamingJSON + `,"model":{"input":["image"]}}}`), nil
				}
				return json.RawMessage(`{"type":"response","success":true}`), nil
			}
			m := piManagerWithSession("root", client, rootTree("root"))
			if err := m.SendMessage(context.Background(), "root", "inspect", []attachments.Image{testPNG(t, 1)}); err == nil {
				t.Fatal("accepted success response without command")
			}
		})
	}
}

func TestSendMessageReturnsExactPiRejection(t *testing.T) {
	client := imageCapablePIControlClient(false)
	client.response = func(idx int, _ string, _ json.RawMessage) (json.RawMessage, error) {
		if idx == 0 {
			return json.RawMessage(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":false,"model":{"input":["text","image"]}}}`), nil
		}
		return json.RawMessage(`{"type":"response","command":"prompt","success":false,"error":"image rejected"}`), nil
	}
	m := piManagerWithSession("root", client, rootTree("root"))

	err := m.SendMessage(context.Background(), "root", "inspect", []attachments.Image{testPNG(t, 1)})
	if err == nil || err.Error() != "pi command failed: image rejected" {
		t.Fatalf("SendMessage() error = %v", err)
	}
}

func TestSteerStreamingUsesSteerCommand(t *testing.T) {
	tree := rootTree("root")
	client := &piControlClient{}
	// First call (get_state) returns isStreaming=true; second call (steer) returns success.
	client.response = func(idx int, _ string, _ json.RawMessage) (json.RawMessage, error) {
		switch idx {
		case 0:
			return json.RawMessage(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":true}}`), nil
		default:
			return json.RawMessage(`{"type":"response","command":"steer","success":true}`), nil
		}
	}
	m := piManagerWithSession("root", client, tree)

	if err := m.Steer(context.Background(), "root", "go"); err != nil {
		t.Fatalf("Steer() error: %v", err)
	}
	if len(client.callLog) != 2 {
		t.Fatalf("expected 2 RPC calls, got %d", len(client.callLog))
	}
	var steerPayload map[string]any
	if err := json.Unmarshal(client.callLog[1].payload, &steerPayload); err != nil {
		t.Fatal(err)
	}
	if steerPayload["type"] != "steer" {
		t.Fatalf("second call type = %q, want steer", steerPayload["type"])
	}
	if steerPayload["message"] != "go" {
		t.Fatalf("message = %v, want go", steerPayload["message"])
	}
}

func TestSteerIdleUsesPromptCommand(t *testing.T) {
	tree := rootTree("root")
	client := &piControlClient{}
	client.response = func(idx int, _ string, _ json.RawMessage) (json.RawMessage, error) {
		switch idx {
		case 0:
			return json.RawMessage(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":false}}`), nil
		default:
			return json.RawMessage(`{"type":"response","command":"prompt","success":true}`), nil
		}
	}
	m := piManagerWithSession("root", client, tree)

	if err := m.Steer(context.Background(), "root", "hello"); err != nil {
		t.Fatalf("Steer() error: %v", err)
	}
	var promptPayload map[string]any
	if err := json.Unmarshal(client.callLog[1].payload, &promptPayload); err != nil {
		t.Fatal(err)
	}
	if promptPayload["type"] != "prompt" {
		t.Fatalf("second call type = %q, want prompt", promptPayload["type"])
	}
}

func TestSteerMessageIsNotInterpolated(t *testing.T) {
	// Message contains JSON-breaking characters — must be safely encoded.
	tree := rootTree("root")
	client := &piControlClient{}
	client.response = func(idx int, _ string, _ json.RawMessage) (json.RawMessage, error) {
		switch idx {
		case 0:
			return json.RawMessage(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":false}}`), nil
		default:
			return json.RawMessage(`{"type":"response","command":"prompt","success":true}`), nil
		}
	}
	m := piManagerWithSession("root", client, tree)
	dangerous := `"injected":true,"type":"evil`
	if err := m.Steer(context.Background(), "root", dangerous); err != nil {
		t.Fatalf("Steer() error: %v", err)
	}
	// Verify the payload is valid JSON (message is encoded, not interpolated).
	var check map[string]any
	if err := json.Unmarshal(client.callLog[1].payload, &check); err != nil {
		t.Fatalf("payload is not valid JSON: %v\npayload: %s", err, client.callLog[1].payload)
	}
	if check["type"] != "prompt" {
		t.Fatalf("injection succeeded: type = %v", check["type"])
	}
}

func TestInterruptSendsAbort(t *testing.T) {
	tree := rootTree("root")
	client := &piControlClient{}
	client.response = func(_ int, _ string, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"type":"response","command":"abort","success":true}`), nil
	}
	m := piManagerWithSession("root", client, tree)

	if err := m.Interrupt(context.Background(), "root"); err != nil {
		t.Fatalf("Interrupt() error: %v", err)
	}
	if len(client.callLog) != 1 {
		t.Fatalf("expected 1 call, got %d", len(client.callLog))
	}
	var abortPayload map[string]any
	if err := json.Unmarshal(client.callLog[0].payload, &abortPayload); err != nil {
		t.Fatal(err)
	}
	if abortPayload["type"] != "abort" {
		t.Fatalf("type = %v, want abort", abortPayload["type"])
	}
}

func TestStopSessionCallsClientStop(t *testing.T) {
	tree := rootTree("root")
	stopClient := &fakeClient{snapshot: tree}
	m := fakeManager(nil)
	handle := &rootHandle{
		socketPath: "/tmp/root.root.sock",
		rootID:     "root",
		actionable: true,
		client:     stopClient,
		tree:       tree,
	}
	m.roots[handle.socketPath] = handle
	m.routes["root"] = "root"

	if err := m.StopSession(context.Background(), "root"); err != nil {
		t.Fatalf("StopSession() error: %v", err)
	}
	stopClient.mu.Lock()
	defer stopClient.mu.Unlock()
	found := false
	for _, call := range stopClient.callLog {
		if call == "Stop" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Stop was not called: %v", stopClient.callLog)
	}
}

func TestAnswerQuestionForwardsToClient(t *testing.T) {
	tree := rootTree("root")
	fc := &fakeClient{snapshot: tree}
	m := fakeManager(nil)
	handle := &rootHandle{
		socketPath: "/tmp/root.root.sock",
		rootID:     "root",
		actionable: true,
		client:     fc,
		tree:       tree,
	}
	m.roots[handle.socketPath] = handle
	m.routes["root"] = "root"

	answer := json.RawMessage(`{"confirmed":true}`)
	if err := m.AnswerQuestion(context.Background(), "root", "q-1", answer); err != nil {
		t.Fatalf("AnswerQuestion() error: %v", err)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	found := false
	for _, call := range fc.callLog {
		if call == "AnswerQuestion" {
			found = true
		}
	}
	if !found {
		t.Fatalf("AnswerQuestion was not called: %v", fc.callLog)
	}
}

// ---- SessionStats tests ----

func TestSessionStatsNullableContextUsage(t *testing.T) {
	tree := rootTree("root")
	client := &piControlClient{}

	tests := []struct {
		name    string
		raw     string
		wantCtx bool
	}{
		{
			name:    "absent_context_usage",
			raw:     `{"type":"response","command":"get_session_stats","success":true,"data":{"sessionId":"pi-root","userMessages":1}}`,
			wantCtx: false,
		},
		{
			name:    "null_tokens_and_percent",
			raw:     `{"type":"response","command":"get_session_stats","success":true,"data":{"sessionId":"pi-root","contextUsage":{"tokens":null,"contextWindow":200000,"percent":null}}}`,
			wantCtx: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client.response = func(_ int, _ string, _ json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(tc.raw), nil
			}
			m := piManagerWithSession("root", client, tree)
			stats, err := m.SessionStats(context.Background(), "root")
			if err != nil {
				t.Fatalf("SessionStats() error: %v", err)
			}
			if tc.wantCtx && stats.ContextUsage == nil {
				t.Fatal("expected non-nil ContextUsage")
			}
			if !tc.wantCtx && stats.ContextUsage != nil {
				t.Fatal("expected nil ContextUsage")
			}
			if tc.wantCtx {
				if stats.ContextUsage.Tokens != nil {
					t.Fatalf("Tokens should be nil, got %v", *stats.ContextUsage.Tokens)
				}
				if stats.ContextUsage.Percent != nil {
					t.Fatalf("Percent should be nil, got %v", *stats.ContextUsage.Percent)
				}
			}
		})
	}
}

func TestSessionStatsFieldMapping(t *testing.T) {
	tree := rootTree("root")
	client := &piControlClient{}
	client.response = func(_ int, _ string, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"type":"response","command":"get_session_stats","success":true,"data":{
			"sessionId":"pi-root",
			"userMessages":3,"assistantMessages":3,"toolCalls":2,"toolResults":2,"totalMessages":10,
			"inputTokens":1000,"outputTokens":500,"cacheReadTokens":200,"cacheWriteTokens":100,"totalTokens":1800,
			"cost":0.025
		}}`), nil
	}
	m := piManagerWithSession("root", client, tree)
	stats, err := m.SessionStats(context.Background(), "root")
	if err != nil {
		t.Fatalf("SessionStats() error: %v", err)
	}
	if stats.UserMessages != 3 || stats.AssistantMessages != 3 {
		t.Fatalf("message counts wrong: %+v", stats)
	}
	if stats.Tokens.Input != 1000 || stats.Tokens.Output != 500 {
		t.Fatalf("token counts wrong: %+v", stats.Tokens)
	}
	if stats.Cost != 0.025 {
		t.Fatalf("cost = %f, want 0.025", stats.Cost)
	}
}

func TestSessionStatsIdentityMismatch(t *testing.T) {
	tree := rootTree("root") // PiSessionID = "pi-root"
	client := &piControlClient{}
	client.response = func(_ int, _ string, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"type":"response","command":"get_session_stats","success":true,"data":{"sessionId":"pi-wrong"}}`), nil
	}
	m := piManagerWithSession("root", client, tree)
	_, err := m.SessionStats(context.Background(), "root")
	if err == nil {
		t.Fatal("expected identity mismatch error")
	}
}

func TestPiControlsRejectNonActionableRoot(t *testing.T) {
	tree := rootTree("root")
	client := &piControlClient{}
	m := fakeManager(nil)
	handle := &rootHandle{
		socketPath: "/tmp/root.root.sock",
		rootID:     "root",
		actionable: false, // non-actionable (stopping)
		client:     client,
		tree:       tree,
	}
	m.roots[handle.socketPath] = handle
	m.routes["root"] = "root"

	if err := m.Interrupt(context.Background(), "root"); err == nil {
		t.Fatal("Interrupt accepted non-actionable root")
	}
	// Stop on a non-actionable root evicts it from the fleet immediately.
	if err := m.StopSession(context.Background(), "root"); err != nil {
		t.Fatalf("StopSession on non-actionable root should evict it: %v", err)
	}
	if len(m.roots) != 0 || len(m.routes) != 0 {
		t.Fatalf("StopSession on non-actionable root did not evict it (roots=%d routes=%d)", len(m.roots), len(m.routes))
	}
	_, err := m.SessionStats(context.Background(), "root")
	if err == nil {
		t.Fatal("SessionStats accepted non-actionable root")
	}
}

func TestPiControlsRejectStaleRoot(t *testing.T) {
	tree := rootTree("root")
	client := &piControlClient{}
	m := piManagerWithSession("root", client, tree)
	for _, handle := range m.roots {
		handle.stale = true
	}

	if err := m.Interrupt(context.Background(), "root"); err == nil {
		t.Fatal("Interrupt accepted stale root")
	}
	// Stop on a stale root evicts it from the fleet immediately instead of
	// waiting for the next discovery pass.
	if err := m.StopSession(context.Background(), "root"); err != nil {
		t.Fatalf("StopSession on stale root should evict it: %v", err)
	}
	if len(m.roots) != 0 || len(m.routes) != 0 {
		t.Fatalf("StopSession on stale root did not evict it (roots=%d routes=%d)", len(m.roots), len(m.routes))
	}
	if err := m.AnswerQuestion(context.Background(), "root", "q-1", json.RawMessage(`{"confirmed":true}`)); err == nil {
		t.Fatal("AnswerQuestion accepted stale root")
	}
	if _, err := m.SessionStats(context.Background(), "root"); err == nil {
		t.Fatal("SessionStats accepted stale root")
	}
	if len(client.callLog) != 0 {
		t.Fatalf("stale controls reached client: %#v", client.callLog)
	}
}

func TestPiControlsRejectUnknownSession(t *testing.T) {
	m := fakeManager(nil)
	if err := m.Interrupt(context.Background(), "missing"); err == nil {
		t.Fatal("Interrupt accepted unknown session")
	}
}

// ---- mustJSON correctness ----

func TestMustJSONEncodesFieldsSafely(t *testing.T) {
	msg := `"injected":true,"type":"evil`
	raw := mustJSON(map[string]any{"type": "prompt", "message": msg})
	var check map[string]any
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("mustJSON produced invalid JSON: %v\n%s", err, raw)
	}
	if check["message"] != msg {
		t.Fatalf("message mangled: %v", check["message"])
	}
	if check["type"] != "prompt" {
		t.Fatalf("type = %v", check["type"])
	}
}

func TestSteerSuccessFalseReturnsError(t *testing.T) {
	tree := rootTree("root")
	client := &piControlClient{}
	client.response = func(idx int, _ string, _ json.RawMessage) (json.RawMessage, error) {
		switch idx {
		case 0:
			return json.RawMessage(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":true}}`), nil
		default:
			return json.RawMessage(`{"type":"response","command":"steer","success":false,"error":"race"}`), nil
		}
	}
	m := piManagerWithSession("root", client, tree)
	if err := m.Steer(context.Background(), "root", "msg"); err == nil {
		t.Fatal("expected error on success:false steer, got nil")
	}
}
