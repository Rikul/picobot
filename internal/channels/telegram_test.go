package channels

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/local/picobot/internal/chat"
	"github.com/local/picobot/internal/config"
)

func TestStartTelegramWithBase(t *testing.T) {
	token := "testtoken"
	// channel to capture sendMessage posts
	sent := make(chan url.Values, 4)

	// simple stateful handler: first getUpdates returns one update, subsequent return empty
	first := true
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/getUpdates") {
			w.Header().Set("Content-Type", "application/json")
			if first {
				first = false
				w.Write([]byte(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":1,"from":{"id":123},"chat":{"id":456,"type":"private"},"text":"hello"}}]}`))
				return
			}
			w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		if strings.HasSuffix(path, "/sendMessage") {
			r.ParseForm()
			sent <- r.PostForm
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"result":{}}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer h.Close()

	base := h.URL + "/bot" + token
	b := chat.NewHub(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := StartTelegramWithBase(ctx, b, token, base, nil, nil); err != nil {
		t.Fatalf("StartTelegramWithBase failed: %v", err)
	}
	// Start the hub router so outbound messages sent to b.Out are dispatched
	// to each channel's subscription (telegram in this test).
	b.StartRouter(ctx)

	// Wait for inbound from getUpdates
	select {
	case msg := <-b.In:
		if msg.Content != "hello" {
			t.Fatalf("unexpected inbound content: %s", msg.Content)
		}
		if msg.ChatID != "456" {
			t.Fatalf("unexpected chat id: %s", msg.ChatID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for inbound message")
	}

	// send an outbound message and ensure server receives it
	out := chat.Outbound{Channel: "telegram", ChatID: "456", Content: "reply"}
	b.Out <- out

	select {
	case v := <-sent:
		if v.Get("chat_id") != "456" || v.Get("text") != "reply" {
			t.Fatalf("unexpected sendMessage form: %v", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for sendMessage to be posted")
	}

	// cancel and allow goroutines to stop
	cancel()
	// give a small grace period
	time.Sleep(50 * time.Millisecond)
}

func TestStartTelegramVoiceTranscription(t *testing.T) {
	token := "testtoken"
	const transcribedText = "this is the transcribed voice message"

	// Track which endpoints were called.
	var getFileCalled, downloadCalled, transcribeCalled bool

	// Fake Telegram server handles getUpdates (returns a voice message), getFile, and file download.
	telegramServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(path, "/getUpdates"):
			if !getFileCalled {
				// Return a single voice update.
				w.Write([]byte(`{"ok":true,"result":[{"update_id":10,"message":{"message_id":2,"from":{"id":42},"chat":{"id":99,"type":"private"},"voice":{"file_id":"fileid123","duration":3}}}]}`))
				return
			}
			w.Write([]byte(`{"ok":true,"result":[]}`))

		case strings.HasSuffix(path, "/getFile"):
			getFileCalled = true
			w.Write([]byte(`{"ok":true,"result":{"file_path":"voice/file.ogg"}}`))

		case strings.Contains(path, "/file/bot") && strings.HasSuffix(path, "/voice/file.ogg"):
			downloadCalled = true
			w.Header().Set("Content-Type", "audio/ogg")
			w.Write([]byte("fake-ogg-audio-data"))

		default:
			w.WriteHeader(404)
		}
	}))
	defer telegramServer.Close()

	// Fake transcription server.
	transcriptionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/audio/transcriptions") {
			transcribeCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"text":"` + transcribedText + `"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer transcriptionServer.Close()

	base := telegramServer.URL + "/bot" + token
	b := chat.NewHub(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	transcriptionCfg := &config.TranscriptionConfig{
		APIBase: transcriptionServer.URL,
		APIKey:  "test-key",
		Model:   "whisper-1",
	}

	if err := StartTelegramWithBase(ctx, b, token, base, nil, transcriptionCfg); err != nil {
		t.Fatalf("StartTelegramWithBase failed: %v", err)
	}
	b.StartRouter(ctx)

	select {
	case msg := <-b.In:
		if msg.Content != transcribedText {
			t.Fatalf("expected transcribed content %q, got %q", transcribedText, msg.Content)
		}
		if msg.ChatID != "99" {
			t.Fatalf("unexpected chat id: %s", msg.ChatID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for transcribed voice message")
	}

	if !getFileCalled {
		t.Error("expected getFile to be called")
	}
	if !downloadCalled {
		t.Error("expected voice file download to be called")
	}
	if !transcribeCalled {
		t.Error("expected transcription endpoint to be called")
	}

	cancel()
	time.Sleep(50 * time.Millisecond)
}

func TestTranscribeViaCLI(t *testing.T) {
	const expected = "hello from whisper"

	// Build a fake whisper script: reads --output_dir and writes <basename>.txt there.
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "fake-whisper")
	script := `#!/bin/sh
# Parse arguments to find the audio file and --output_dir value.
audio=""
outdir=""
prev=""
for arg; do
  case "$prev" in
    --output_dir) outdir="$arg" ;;
  esac
  case "$arg" in
    --*) ;;
    *) if [ -z "$audio" ]; then audio="$arg"; fi ;;
  esac
  prev="$arg"
done
base=$(basename "$audio")
base="${base%.*}"
printf 'hello from whisper' > "$outdir/$base.txt"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake whisper script: %v", err)
	}

	ctx := context.Background()
	got, err := transcribeViaCLI(ctx, []byte("fake-audio"), "voice.ogg", &config.TranscriptionConfig{
		Command: scriptPath,
		Model:   "tiny",
	})
	if err != nil {
		t.Fatalf("transcribeViaCLI: %v", err)
	}
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}
