package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type TelegramNotifier struct {
	token   string
	chatID  string
	client  *http.Client
	enabled bool
}

func NewTelegramNotifierFromEnv() *TelegramNotifier {
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	chatID := strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))

	n := &TelegramNotifier{
		token:  token,
		chatID: chatID,
		client: &http.Client{Timeout: 6 * time.Second},
	}

	// 不强制要求存在：你也可以改成强制（缺就退出）
	// 这里默认：缺任意一个就禁用，不影响 controller 工作
	if token != "" && chatID != "" {
		n.enabled = true
	}
	return n
}

func (n *TelegramNotifier) Enabled() bool { return n.enabled }

func (n *TelegramNotifier) Send(ctx context.Context, text string) error {
	if !n.enabled {
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.token)

	body := map[string]interface{}{
		"chat_id":                  n.chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}

	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("telegram notify: failed to close response body: %v\n", err)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram sendMessage failed: status=%s", resp.Status)
	}
	return nil
}
