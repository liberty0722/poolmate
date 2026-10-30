// Package notify 告警通知中心：企业微信机器人 / Bark / Server酱 / Telegram /
// 自定义 Webhook 五通道并行投递，同类事件窗口去重，投递失败不阻塞主流程。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"poolmate/internal/config"
)

// Event 一条告警事件。
type Event struct {
	Kind  string `json:"kind"` // account_disabled / breaker_open / auth_failed / low_balance / token_expiring / waste_forecast / budget_exceeded / task_failed / growth_claim / test
	Title string `json:"title"`
	Body  string `json:"body"`
	UID   string `json:"uid,omitempty"`
	Level string `json:"level"` // info / warn / critical
}

// Notifier 通知器。
type Notifier struct {
	mu     sync.Mutex
	cfg    *config.Config
	client *http.Client
	sent   map[string]time.Time // dedupKey → 上次投递
	log    []Event              // 最近 100 条（面板可见）
}

func New(cfg *config.Config) *Notifier {
	return &Notifier{
		cfg: cfg, sent: map[string]time.Time{},
		client: &http.Client{Timeout: 8 * time.Second},
	}
}

// Emit 投递事件（去重窗口内同类同对象只发一次）。非阻塞：失败仅记日志。
func (n *Notifier) Emit(e Event) {
	c := n.cfg.Snapshot().Notify
	if !c.Enabled {
		n.record(e)
		return
	}
	if e.Level == "" {
		e.Level = "warn"
	}
	dedup := time.Duration(c.DedupMinutes) * time.Minute
	if dedup <= 0 {
		dedup = time.Hour
	}
	key := e.Kind + "|" + e.UID
	n.mu.Lock()
	if last, ok := n.sent[key]; ok && time.Since(last) < dedup {
		n.mu.Unlock()
		return
	}
	n.sent[key] = time.Now()
	n.recordLocked(e)
	n.mu.Unlock()
	go n.dispatch(e, c)
}

func (n *Notifier) record(e Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.recordLocked(e)
}

func (n *Notifier) recordLocked(e Event) {
	n.log = append([]Event{e}, n.log...)
	if len(n.log) > 100 {
		n.log = n.log[:100]
	}
}

// Recent 最近告警记录。
func (n *Notifier) Recent() []Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Event, len(n.log))
	copy(out, n.log)
	return out
}

// Test 发送测试消息，返回各通道结果。
func (n *Notifier) Test() map[string]string {
	c := n.cfg.Snapshot().Notify
	e := Event{Kind: "test", Level: "info", Title: "PoolMate 测试通知",
		Body: fmt.Sprintf("通道连通性测试 %s", time.Now().Format("15:04:05"))}
	return n.dispatch(e, c)
}

func (n *Notifier) dispatch(e Event, c config.NotifyCfg) map[string]string {
	results := map[string]string{}
	text := e.Title + "\n" + e.Body
	send := func(name string, fn func() error) {
		if err := fn(); err != nil {
			results[name] = err.Error()
		} else {
			results[name] = "ok"
		}
	}
	if c.WecomWebhook != "" {
		send("wecom", func() error { return n.sendWecom(c.WecomWebhook, text) })
	}
	if c.BarkURL != "" {
		send("bark", func() error { return n.sendBark(c.BarkURL, e.Title, e.Body) })
	}
	if c.ServerChanKey != "" {
		send("serverchan", func() error { return n.sendServerChan(c.ServerChanKey, e.Title, e.Body) })
	}
	if c.TelegramToken != "" && c.TelegramChatID != "" {
		send("telegram", func() error { return n.sendTelegram(c.TelegramToken, c.TelegramChatID, text) })
	}
	if c.CustomURL != "" {
		send("custom", func() error { return n.sendCustom(c.CustomURL, e) })
	}
	return results
}

func (n *Notifier) sendWecom(hook, text string) error {
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]string{"content": "**PoolMate 告警**\n> " + strings.ReplaceAll(text, "\n", "\n> ")},
	}
	raw, _ := json.Marshal(payload)
	resp, err := n.client.Post(hook, "application/json", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("wecom %d: %s", resp.StatusCode, b)
	}
	return nil
}

func (n *Notifier) sendBark(base, title, body string) error {
	u := strings.TrimSuffix(base, "/") + "/" + url.PathEscape(title) + "/" + url.PathEscape(body)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("bark %d", resp.StatusCode)
	}
	return nil
}

func (n *Notifier) sendServerChan(key, title, body string) error {
	form := url.Values{"title": {title}, "desp": {body}}
	resp, err := n.client.PostForm("https://sctapi.ftqq.com/"+key+".send", form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("serverchan %d", resp.StatusCode)
	}
	return nil
}

func (n *Notifier) sendTelegram(token, chatID, text string) error {
	u := "https://api.telegram.org/bot" + token + "/sendMessage"
	payload, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text})
	resp, err := n.client.Post(u, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram %d: %s", resp.StatusCode, b)
	}
	return nil
}

func (n *Notifier) sendCustom(u string, e Event) error {
	payload, _ := json.Marshal(map[string]any{
		"source": "poolmate", "time": time.Now().Format(time.RFC3339),
		"kind": e.Kind, "level": e.Level, "title": e.Title, "body": e.Body, "uid": e.UID,
	})
	resp, err := n.client.Post(u, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("custom %d", resp.StatusCode)
	}
	return nil
}
