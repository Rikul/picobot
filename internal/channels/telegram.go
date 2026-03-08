package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

// transcribeVoice downloads a Telegram voice/audio file and transcribes it via the whisper CLI.
func transcribeVoice(ctx context.Context, client *http.Client, telegramBase, fileBase, fileID string, cfg *config.TranscriptionConfig) (string, error) {
	// Resolve the Telegram file path.
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

	// Download the audio file.
	dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fileBase+"/"+gf.Result.FilePath, nil)
	if err != nil {
		return "", fmt.Errorf("build download request: %w", err)
	}
	dlResp, err := client.Do(dlReq)
	if err != nil {
		return "", fmt.Errorf("download voice file: %w", err)
	}
	audioData, _ := io.ReadAll(dlResp.Body)
	dlResp.Body.Close()

	// Derive a filename from the Telegram path (preserves extension like .ogg, .mp3).
	filename := "voice.ogg"
	if parts := strings.Split(gf.Result.FilePath, "/"); len(parts) > 0 {
		filename = parts[len(parts)-1]
	}

	return transcribeViaCLI(ctx, audioData, filename, cfg)
}

// transcribeViaCLI writes the audio to a temp file, invokes the whisper CLI,
// and reads the resulting .txt output file.
//
// Compatible with `openai-whisper` (pip install openai-whisper):
//
//	whisper <file> --model tiny --output_format txt --output_dir <dir>
func transcribeViaCLI(ctx context.Context, audioData []byte, filename string, cfg *config.TranscriptionConfig) (string, error) {
	// Write audio to a temp file.
	tmpAudio, err := os.CreateTemp("", "tg-voice-*-"+filename)
	if err != nil {
		return "", fmt.Errorf("create temp audio file: %w", err)
	}
	defer os.Remove(tmpAudio.Name())
	if _, err := tmpAudio.Write(audioData); err != nil {
		tmpAudio.Close()
		return "", fmt.Errorf("write temp audio: %w", err)
	}
	tmpAudio.Close()

	// Temp dir for whisper output.
	outDir, err := os.MkdirTemp("", "whisper-out-*")
	if err != nil {
		return "", fmt.Errorf("create whisper output dir: %w", err)
	}
	defer os.RemoveAll(outDir)

	model := cfg.Model
	if model == "" {
		model = "tiny"
	}

	// Build command. Support "python3 -m whisper" style multi-word commands.
	parts := strings.Fields(cfg.Command)
	args := append(parts[1:], tmpAudio.Name(),
		"--model", model,
		"--output_format", "txt",
		"--output_dir", outDir,
	)
	cmd := exec.CommandContext(ctx, parts[0], args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("whisper CLI: %w\n%s", err, out)
	}

	// Whisper writes <basename>.txt in outDir.
	base := strings.TrimSuffix(filepath.Base(tmpAudio.Name()), filepath.Ext(tmpAudio.Name()))
	txt, err := os.ReadFile(filepath.Join(outDir, base+".txt"))
	if err != nil {
		return "", fmt.Errorf("read whisper output: %w", err)
	}
	return strings.TrimSpace(string(txt)), nil
}

