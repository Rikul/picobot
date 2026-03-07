package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/local/picobot/internal/chat"
	"github.com/local/picobot/internal/config"
)

// StartTelegram is a convenience wrapper that uses the real polling implementation
// with the standard Telegram base URL.
// allowFrom is a list of Telegram user IDs permitted to interact with the bot.
// If empty, ALL users are allowed (open mode).
func StartTelegram(ctx context.Context, hub *chat.Hub, token string, allowFrom []string, transcriptionCfg *config.TranscriptionConfig) error {
	if token == "" {
		return fmt.Errorf("telegram token not provided")
	}
	base := "https://api.telegram.org/bot" + token
	return StartTelegramWithBase(ctx, hub, token, base, allowFrom, transcriptionCfg)
}

// StartTelegramWithBase starts long-polling against the given base URL (e.g., https://api.telegram.org/bot<TOKEN> or a test server URL).
// allowFrom restricts which Telegram user IDs may send messages. Empty means allow all.
func StartTelegramWithBase(ctx context.Context, hub *chat.Hub, token, base string, allowFrom []string, transcriptionCfg *config.TranscriptionConfig) error {
	if base == "" {
		return fmt.Errorf("base URL is required")
	}

	// Build a fast lookup set for allowed user IDs.
	allowed := make(map[string]struct{}, len(allowFrom))
	for _, id := range allowFrom {
		allowed[id] = struct{}{}
	}

	// fileBase is the Telegram file download base URL, derived from the bot API base.
	// e.g. https://api.telegram.org/bot<TOKEN> → https://api.telegram.org/file/bot<TOKEN>
	fileBase := strings.Replace(base, "/bot", "/file/bot", 1)

	client := &http.Client{Timeout: 45 * time.Second}

	// inbound polling goroutine
	go func() {
		offset := int64(0)
		for {
			select {
			case <-ctx.Done():
				log.Println("telegram: stopping inbound polling")
				return
			default:
			}

			values := url.Values{}
			values.Set("offset", strconv.FormatInt(offset, 10))
			values.Set("timeout", "30")
			u := base + "/getUpdates"
			resp, err := client.PostForm(u, values)
			if err != nil {
				log.Printf("telegram getUpdates error: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var gu struct {
				Ok     bool `json:"ok"`
				Result []struct {
					UpdateID int64 `json:"update_id"`
					Message  *struct {
						MessageID int64 `json:"message_id"`
						From      *struct {
							ID int64 `json:"id"`
						} `json:"from"`
						Chat struct {
							ID int64 `json:"id"`
						} `json:"chat"`
						Text  string `json:"text"`
						Voice *struct {
							FileID   string `json:"file_id"`
							Duration int    `json:"duration"`
						} `json:"voice"`
						Audio *struct {
							FileID   string `json:"file_id"`
							Duration int    `json:"duration"`
						} `json:"audio"`
					} `json:"message"`
				} `json:"result"`
			}
			if err := json.Unmarshal(body, &gu); err != nil {
				log.Printf("telegram: invalid getUpdates response: %v", err)
				continue
			}
			for _, upd := range gu.Result {
				if upd.UpdateID >= offset {
					offset = upd.UpdateID + 1
				}
				if upd.Message == nil {
					continue
				}
				m := upd.Message
				fromID := ""
				if m.From != nil {
					fromID = strconv.FormatInt(m.From.ID, 10)
				}
				// Enforce allowFrom: if the list is non-empty, reject unknown senders.
				if len(allowed) > 0 {
					if _, ok := allowed[fromID]; !ok {
						log.Printf("telegram: dropping message from unauthorized user %s", fromID)
						continue
					}
				}
				chatID := strconv.FormatInt(m.Chat.ID, 10)

				content := m.Text

				// Handle voice and audio messages via transcription.
				if content == "" {
					var fileID string
					if m.Voice != nil {
						fileID = m.Voice.FileID
					} else if m.Audio != nil {
						fileID = m.Audio.FileID
					}
					if fileID != "" {
						if transcriptionCfg == nil {
							log.Printf("telegram: received voice/audio message but transcription is not configured, dropping")
							continue
						}
						transcribed, err := transcribeVoice(ctx, client, base, fileBase, fileID, transcriptionCfg)
						if err != nil {
							log.Printf("telegram: transcription failed: %v", err)
							continue
						}
						content = transcribed
					}
				}

				if content == "" {
					continue
				}

				hub.In <- chat.Inbound{
					Channel:   "telegram",
					SenderID:  fromID,
					ChatID:    chatID,
					Content:   content,
					Timestamp: time.Now(),
				}
			}
		}
	}()

	// Subscribe to the outbound queue before launching the goroutine so the
	// registration is visible to the hub router from the moment this function returns.
	outCh := hub.Subscribe("telegram")

	// outbound sender goroutine
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		for {
			select {
			case <-ctx.Done():
				log.Println("telegram: stopping outbound sender")
				return
			case out := <-outCh:
				u := base + "/sendMessage"
				v := url.Values{}
				v.Set("chat_id", out.ChatID)
				v.Set("text", out.Content)
				resp, err := client.PostForm(u, v)
				if err != nil {
					log.Printf("telegram sendMessage error: %v", err)
					continue
				}
				io.ReadAll(resp.Body)
				resp.Body.Close()
			}
		}
	}()

	return nil
}

// transcribeVoice downloads the Telegram file identified by fileID and transcribes
// it using the configured Whisper-compatible API endpoint.
func transcribeVoice(ctx context.Context, client *http.Client, telegramBase, fileBase, fileID string, cfg *config.TranscriptionConfig) (string, error) {
	// Step 1: resolve the file path via Telegram's getFile endpoint.
	resp, err := client.PostForm(telegramBase+"/getFile", url.Values{"file_id": {fileID}})
	if err != nil {
		return "", fmt.Errorf("getFile request: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var gf struct {
		Ok     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &gf); err != nil || !gf.Ok || gf.Result.FilePath == "" {
		return "", fmt.Errorf("getFile invalid response: %s", body)
	}

	// Step 2: download the audio file.
	fileURL := fileBase + "/" + gf.Result.FilePath
	dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return "", fmt.Errorf("build download request: %w", err)
	}
	dlResp, err := client.Do(dlReq)
	if err != nil {
		return "", fmt.Errorf("download voice file: %w", err)
	}
	audioData, _ := io.ReadAll(dlResp.Body)
	dlResp.Body.Close()

	// Step 3: build multipart form and POST to the transcription API.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	model := cfg.Model
	if model == "" {
		model = "whisper-1"
	}
	if err := mw.WriteField("model", model); err != nil {
		return "", fmt.Errorf("write model field: %w", err)
	}

	// Use the filename from the Telegram file path to preserve the extension.
	filename := "voice.ogg"
	if parts := strings.Split(gf.Result.FilePath, "/"); len(parts) > 0 {
		filename = parts[len(parts)-1]
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(audioData); err != nil {
		return "", fmt.Errorf("write audio data: %w", err)
	}
	mw.Close()

	apiBase := strings.TrimRight(cfg.APIBase, "/")
	tReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/audio/transcriptions", &buf)
	if err != nil {
		return "", fmt.Errorf("build transcription request: %w", err)
	}
	tReq.Header.Set("Content-Type", mw.FormDataContentType())
	if cfg.APIKey != "" {
		tReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	tClient := &http.Client{Timeout: 60 * time.Second}
	tResp, err := tClient.Do(tReq)
	if err != nil {
		return "", fmt.Errorf("transcription request: %w", err)
	}
	tBody, _ := io.ReadAll(tResp.Body)
	tResp.Body.Close()

	var tr struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(tBody, &tr); err != nil {
		return "", fmt.Errorf("transcription parse error (%s): %w", tBody, err)
	}
	if tr.Text == "" {
		return "", fmt.Errorf("transcription returned empty text: %s", tBody)
	}
	return tr.Text, nil
}
