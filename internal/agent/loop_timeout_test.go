package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/local/picobot/internal/chat"
	"github.com/local/picobot/internal/providers"
)

// blockingProvider waits until the request context is cancelled.
type blockingProvider struct{}

func (p *blockingProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string) (providers.LLMResponse, error) {
	<-ctx.Done()
	return providers.LLMResponse{}, ctx.Err()
}

func (p *blockingProvider) GetDefaultModel() string { return "blocking" }

func TestAgentRunHonorsTimeout(t *testing.T) {
	hub := chat.NewHub(10)
	ag := NewAgentLoop(hub, &blockingProvider{}, "blocking", 3, t.TempDir(), nil, nil)
	ag.SetToolActivityIndicator(false)
	ag.SetAgentTimeout(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	select {
	case hub.In <- chat.Inbound{Channel: "cli", SenderID: "user", ChatID: "one", Content: "hello"}:
	case <-time.After(time.Second):
		t.Fatal("could not send inbound message")
	}

	select {
	case out := <-hub.Out:
		if !strings.Contains(out.Content, "timed out") {
			t.Fatalf("expected timeout message, got %q", out.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for gateway timeout response")
	}
}

func TestProcessDirectHonorsTimeout(t *testing.T) {
	hub := chat.NewHub(10)
	ag := NewAgentLoop(hub, &blockingProvider{}, "blocking", 3, t.TempDir(), nil, nil)
	ag.SetToolActivityIndicator(false)

	_, err := ag.ProcessDirect("hello", 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}
