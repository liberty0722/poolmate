// Package upstream 封装 WorkBuddy（CodeBuddy）上游协议：
// 设备授权登录、token 刷新、余额/积分包、每日签到、模型目录与 chat SSE。
// 端点与官方 CLI 对齐，请求头携带官方客户端指纹族。
package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"poolmate/internal/auth"
)

const (
	ChatBaseCN    = "https://copilot.tencent.com"
	BillingBaseCN = "https://www.codebuddy.cn"
	GlobalBase    = "https://www.workbuddy.ai"

	chatPath          = "/v2/chat/completions"
	refreshPath       = "/v2/plugin/auth/token/refresh"
	authStatePath     = "/v2/plugin/auth/state?platform=CLI"
	authTokenPath     = "/v2/plugin/auth/token?state="
	loginAccountPath  = "/v2/plugin/login/account?state="
	billingMeterPath  = "/billing/meter/get-user-resource"
	checkinPath       = "/billing/meter/daily-checkin"
	billingMeterV2    = "/v2" + billingMeterPath
	checkinV2         = "/v2" + checkinPath
	globalModelsPath  = "/v2/enterprises/personal/models"
	v3ConfigPath      = "/v3/config"
	packageEndLayout  = "2006-01-02 15:04:05"
	defaultUAChatCN   = "WorkBuddy/2.63.2 WorkBuddy/2.63.2 CLI/2.63.2"
	defaultUAChatGlob = "WorkBuddy AI/2.63.2 WorkBuddy AI/2.63.2 CLI/2.63.2"
	defaultUABilling  = "WorkBuddy/2.63.2"
	clientVersion     = "2.63.2"
)

// ErrKind 上游错误的治理分类。
type ErrKind int

const (
	ErrUnknown ErrKind = iota
	ErrNetwork
	ErrRateLimit      // 429 软冷却
	ErrNoCredit       // 402 硬冷却至次日 04:00
	ErrAuth           // 401/403 token 失效
	ErrNotFound       // 404 浅冷却
	ErrModelRateLimit // 6004 模型级限流
	ErrContentBlocked // 内容/渠道拦截
	ErrServer         // 5xx
)

// Error 上游错误：HTTP 状态 + 业务码 + 原文。
type Error struct {
	Status int
	Code   int
	Msg    string
	Kind   ErrKind
}

func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("upstream %d code=%d: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("upstream %d: %s", e.Status, e.Msg)
}

// Classify 按状态码/业务码/文案归类。
func Classify(status, code int, msg string) ErrKind {
	m := strings.ToLower(msg)
	switch {
	case code == 6004:
		return ErrModelRateLimit
	case code == 11128 || strings.Contains(m, "unapproved channel") || strings.Contains(m, "illegal api invocation"):
		return ErrContentBlocked
	case status == 429:
		return ErrRateLimit
	case status == 402 || strings.Contains(m, "余额不足") || strings.Contains(m, "insufficient"):
		return ErrNoCredit
	case status == 401 || status == 403:
		return ErrAuth
	case status == 404:
		return ErrNotFound
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrUnknown
	}
	if strings.Contains(m, "敏感") || strings.Contains(m, "moderation") || strings.Contains(m, "blocked") {
		return ErrContentBlocked
	}
	return ErrUnknown
}

// Client 上游客户端。
type Client struct {
	HTTP *http.Client
}

func New(timeout time.Duration) *Client {
	tr := &http.Transport{
		MaxIdleConns: 64, IdleConnTimeout: 120 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{HTTP: &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 上游偶发把 https 重定向到 http，Go 客户端跟随后会报
			// "server gave HTTP response to HTTPS client"。
			// 这里显式升级回 https，避免协议降级。
			if req.URL.Scheme == "http" {
				req.URL.Scheme = "https"
			}
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return nil
		},
	}}
}

func (c *Client) chatBase(a *auth.Auth) string {
	if a.Snapshot().Realm == "global" {
		return GlobalBase
	}
	return ChatBaseCN
}

func (c *Client) billingBase(a *auth.Auth) string {
	if a.Snapshot().Realm == "global" {
		return GlobalBase
	}
	return BillingBaseCN
}

func uaChat(a *auth.Auth) string {
	if a.Snapshot().Realm == "global" {
		return defaultUAChatGlob
	}
	return defaultUAChatCN
}

// doJSON 解析 {code,msg,data} 业务信封；code!=0 或 HTTP>=400 → *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Kind: ErrNetwork, Msg: err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &Error{Status: resp.StatusCode, Kind: ErrNetwork, Msg: err.Error()}
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if jerr := json.Unmarshal(raw, &env); jerr == nil && (env.Code != 0 || env.Data != nil || env.Msg != "") {
		if env.Code != 0 {
			return nil, &Error{Status: resp.StatusCode, Code: env.Code, Msg: env.Msg, Kind: Classify(resp.StatusCode, env.Code, env.Msg)}
		}
		if resp.StatusCode >= 400 {
			return nil, &Error{Status: resp.StatusCode, Msg: string(raw), Kind: Classify(resp.StatusCode, 0, string(raw))}
		}
		return env.Data, nil
	}
	if resp.StatusCode >= 400 {
		return nil, &Error{Status: resp.StatusCode, Msg: strings.TrimSpace(string(raw)), Kind: Classify(resp.StatusCode, 0, string(raw))}
	}
	return json.RawMessage(raw), nil
}

// deriveStableID 以 uid 派生设备/会话标识（同账号不漂移、跨账号不关联）。
func deriveStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("poolmate|" + purpose + "|" + uid))
	return hex.EncodeToString(sum[:16])
}

// chatHeaders 注入官方客户端头族（指纹对齐真实桌面端）。
func (c *Client) chatHeaders(req *http.Request, a *auth.Auth, meta ChatMeta) {
	s := a.Snapshot()
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	if s.UID != "" {
		req.Header.Set("X-User-Id", s.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if s.Realm == "global" {
		req.Header.Set("X-Domain", "www.workbuddy.ai")
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Machine-ID", deriveStableID(s.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveStableID(s.UID, "session"))
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", clientVersion)
	req.Header.Set("X-Product", "WorkBuddy")
	req.Header.Set("User-Agent", uaChat(a))
	// 会话头族：对话轮级聚合主键必发，消息级每条独立。
	convReqID := randHex16()
	msgID := randHex16()
	if meta.ConversationID != "" {
		req.Header.Set("X-Conversation-ID", meta.ConversationID)
	}
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", msgID)
	req.Header.Set("X-Request-ID", msgID)
	req.Header.Set("X-Root-Request-ID", convReqID)
	req.Header.Set("X-Trace-ID", convReqID)
	req.Header.Set("X-B3-TraceId", convReqID)
	req.Header.Set("X-B3-SpanId", msgID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

func (c *Client) billingHeaders(req *http.Request, a *auth.Auth) {
	s := a.Snapshot()
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	if s.UID != "" {
		req.Header.Set("X-User-Id", s.UID)
	}
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
	}
	req.Header.Set("User-Agent", defaultUABilling)
	if s.Realm == "global" {
		req.Header.Set("X-Domain", "www.workbuddy.ai")
	}
}

// ChatMeta 一次 chat 出站的会话元数据。
type ChatMeta struct {
	ConversationID string // 客户端透传的会话 ID，空则不发
}

// RefreshToken 刷新 access token（双 token 轮换，并发安全由 Auth.ApplyTokens 保证）。
func (c *Client) RefreshToken(a *auth.Auth) error {
	s := a.Snapshot()
	if strings.TrimSpace(s.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+refreshPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("X-Refresh-Token", s.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	req.Header.Set("User-Agent", defaultUABilling)
	if s.Realm == "global" {
		req.Header.Set("X-Domain", "www.workbuddy.ai")
	}
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: 响应无 accessToken，需重新授权")
	}
	a.ApplyTokens(s.AccessToken, s.RefreshToken, tok.AccessToken, tok.RefreshToken, tok.Domain, tok.ExpiresIn)
	return nil
}

// resourceAcct 上游 get-user-resource 返回的单个积分包。
type resourceAcct struct {
	PackageName         string `json:"PackageName"`
	CycleEndTime        string `json:"CycleEndTime"`
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
}

// Package 一个积分包的到期与余量。
type Package struct {
	Name       string    `json:"name"`
	Remain     int64     `json:"remain"`
	Used       int64     `json:"used"`
	Size       int64     `json:"size"`
	CycleEnd   time.Time `json:"cycle_end,omitempty"`
	HasCycle   bool      `json:"has_cycle"`
}

// Balance 查询积分余额与积分包明细（Cycle 套餐优先口径）。
func (c *Client) Balance(a *auth.Auth) (int64, []Package, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber": 1, "PageSize": 100, "ProductCode": "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.AddDate(101, 0, 0).Format(packageEndLayout),
	}
	paths := []string{billingMeterV2}
	if a.Snapshot().Realm == "global" {
		paths = []string{billingMeterPath, billingMeterV2}
	}
	data, err := c.meterJSON(a, paths, http.MethodPost, body)
	if err != nil {
		return 0, nil, err
	}
	// 真实响应为 data.Response.Data.Accounts 三层嵌套；
	// 同时兼容 data.Accounts 的扁平形态（防御上游改版）。
	var resp struct {
		TotalDosage int64          `json:"TotalDosage"`
		Accounts    []resourceAcct `json:"Accounts"`
		Response    struct {
			Data struct {
				TotalDosage int64          `json:"TotalDosage"`
				Accounts    []resourceAcct `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, nil, fmt.Errorf("resource parse: %w", err)
	}
	if len(resp.Accounts) == 0 && len(resp.Response.Data.Accounts) > 0 {
		resp.Accounts = resp.Response.Data.Accounts
		resp.TotalDosage = resp.Response.Data.TotalDosage
	}
	var remain int64
	var packs []Package
	for _, ac := range resp.Accounts {
		p := Package{Name: ac.PackageName}
		if ac.CycleCapacitySize > 0 {
			p.Size, p.Remain = ac.CycleCapacitySize, ac.CycleCapacityRemain
			if p.Remain < 0 {
				p.Remain = 0
			}
			if p.Remain > p.Size {
				p.Remain = p.Size
			}
			p.Used = p.Size - p.Remain
			if ac.CycleCapacityUsed > p.Used {
				p.Used = ac.CycleCapacityUsed
			}
		} else {
			p.Size, p.Remain, p.Used = ac.CapacitySize, ac.CapacityRemain, ac.CapacityUsed
			if p.Used == 0 && p.Size > p.Remain {
				p.Used = p.Size - p.Remain
			}
		}
		if t, err := time.ParseInLocation(packageEndLayout, ac.CycleEndTime, time.Local); err == nil {
			p.CycleEnd, p.HasCycle = t, true
		}
		remain += p.Remain
		packs = append(packs, p)
	}
	return remain, packs, nil
}

func (c *Client) meterJSON(a *auth.Auth, paths []string, method string, body any) (json.RawMessage, error) {
	raw, _ := json.Marshal(body)
	var lastErr error
	for i, p := range paths {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		req, err := http.NewRequestWithContext(ctx, method, c.billingBase(a)+p, bytes.NewReader(raw))
		if err != nil {
			cancel()
			return nil, err
		}
		c.billingHeaders(req, a)
		data, err := c.doJSON(req)
		cancel()
		if err == nil {
			return data, nil
		}
		lastErr = err
		if ue, ok := err.(*Error); !ok || ue.Status != 404 || i == len(paths)-1 {
			return nil, err
		}
	}
	return nil, lastErr
}

// DailyCheckin 每日签到（重复签到返回业务错误，用 IsAlreadyCheckin 判定）。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	paths := []string{checkinV2}
	if a.Snapshot().Realm == "global" {
		paths = []string{checkinPath, checkinV2}
	}
	_, err := c.meterJSON(a, paths, http.MethodPost, map[string]any{})
	return err
}

func IsAlreadyCheckin(err error) bool {
	ue, ok := err.(*Error)
	if !ok {
		return false
	}
	m := strings.ToLower(ue.Msg)
	return strings.Contains(ue.Msg, "已签到") || strings.Contains(m, "already")
}

// ChatStream 发起流式 chat。成功返回 SSE body（调用方负责 Close）。
func (c *Client) ChatStream(ctx context.Context, a *auth.Auth, body []byte, meta ChatMeta) (io.ReadCloser, *Error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Kind: ErrUnknown, Msg: err.Error()}
	}
	c.chatHeaders(req, a, meta)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Kind: ErrNetwork, Msg: err.Error()}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		var env struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		code := 0
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &env) == nil {
			code, msg = env.Code, firstNonEmpty(env.Msg, msg)
		}
		return nil, &Error{Status: resp.StatusCode, Code: code, Msg: msg, Kind: Classify(resp.StatusCode, code, msg)}
	}
	return resp.Body, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Model 模型目录条目。
type Model struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	ContextWindow      int      `json:"context_window"`
	MaxOutput          int      `json:"max_output"`
	Rate               float64  `json:"rate"` // 积分倍率
	Reasoning          []string `json:"reasoning,omitempty"`
	DefaultEffort      string   `json:"default_effort,omitempty"`
	CanDisableThinking bool     `json:"can_disable_thinking,omitempty"`
	SupportedLengths   []int    `json:"supported_lengths,omitempty"` // 上游支持可选的上下文长度档
	Description        string   `json:"description,omitempty"`
	Vision             bool     `json:"vision"`
	Tools              bool     `json:"tools"`
}

// Models 拉取模型目录（CN: /v3/config；global: /v2/enterprises/personal/models），解析容错。
func (c *Client) Models(a *auth.Auth) ([]Model, error) {
	s := a.Snapshot()
	path := v3ConfigPath
	base := c.chatBase(a)
	if s.Realm == "global" {
		path = globalModelsPath
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	c.chatHeaders(req, a, ChatMeta{})
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &Error{Status: resp.StatusCode, Msg: string(raw), Kind: Classify(resp.StatusCode, 0, string(raw))}
	}
	return parseModels(raw), nil
}

// parseModels 从任意 JSON 里宽松提取模型条目：递归找含 id/name 字段的对象数组。
func parseModels(raw []byte) []Model {
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Model
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			id := strOf(t, "id", "model", "modelId", "model_id")
			if id == "" {
				id = strOf(t, "name", "modelName", "model_name")
			}
			if id != "" && looksLikeModel(t) && !seen[id] {
				seen[id] = true
				m := Model{
					ID:            id,
					Name:          firstNonEmpty(strOf(t, "name", "displayName", "display_name"), id),
					ContextWindow: intOf(t, "maxInputTokens", "context_window", "contextLength", "context_length"),
					MaxOutput:     intOf(t, "maxOutputTokens", "maxOutput", "max_output", "max_completion_tokens"),
					Rate:          rateOf(t),
					Vision:        boolOf(t, "supportsImages", "vision", "supportsVision", "image", "imageInput"),
					Tools:         boolOf(t, "supportsToolCall", "tools", "supportsTools", "toolCall", "tool_call", "functionCall"),
					Description:   strOf(t, "descriptionZh", "descriptionEn", "description"),
				}
				// reasoning 两种形态：对象 {defaultEffort, supportedEfforts, canDisableThinking} 或字符串数组。
				switch r := t["reasoning"].(type) {
				case map[string]any:
					m.DefaultEffort = strOf(r, "defaultEffort", "default_effort", "effort")
					m.CanDisableThinking = boolOf(r, "canDisableThinking", "can_disable_thinking")
					if arr, ok := r["supportedEfforts"].([]any); ok {
						for _, x := range arr {
							if s, ok := x.(string); ok {
								m.Reasoning = append(m.Reasoning, s)
							}
						}
					}
				case []any:
					for _, x := range r {
						if s, ok := x.(string); ok {
							m.Reasoning = append(m.Reasoning, s)
						}
					}
				}
				// contextWindow 对象形态：{defaultLength, supportedLengths:[...]}。
				if cw, ok := t["contextWindow"].(map[string]any); ok {
					if arr, ok := cw["supportedLengths"].([]any); ok {
						for _, x := range arr {
							if f, ok := x.(float64); ok && f > 0 {
								m.SupportedLengths = append(m.SupportedLengths, int(f))
							}
						}
					}
					if m.ContextWindow == 0 {
						m.ContextWindow = intOf(cw, "defaultLength", "default_length")
					}
				}
				if m.MaxOutput > m.ContextWindow && m.ContextWindow > 0 {
					m.MaxOutput = m.ContextWindow / 2
				}
				out = append(out, m)
			}
			for _, v2 := range t {
				walk(v2)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(root)
	return out
}

func looksLikeModel(t map[string]any) bool {
	for _, k := range []string{"contextWindow", "context_window", "contextLength", "context_length", "maxInputTokens", "maxOutput", "max_output", "maxOutputTokens", "maxAllowedSize", "rate", "credit", "credits", "supportsToolCall"} {
		if _, ok := t[k]; ok {
			return true
		}
	}
	return false
}

// rateOf 解析积分倍率：数值字段优先，其次 "x0.69" 形式的字符串字段（credits）。
func rateOf(t map[string]any) float64 {
	if v := floatOf(t, "rate", "creditRate", "credit_rate", "credit"); v > 0 {
		return v
	}
	for _, k := range []string{"credits", "credit", "rate"} {
		if s, ok := t[k].(string); ok {
			s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "x"), "X")
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

func strOf(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			return v
		}
	}
	return ""
}

func intOf(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return 0
}

func floatOf(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k].(float64); ok {
			return v
		}
	}
	return 0
}

func boolOf(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v
		}
	}
	return false
}

// DeviceAuthURL 设备授权第一步：拿 state 与授权链接。
func (c *Client) DeviceAuthURL(realm string) (string, string, error) {
	base, origin := ChatBaseCN, "https://www.codebuddy.cn"
	if realm == "global" {
		base, origin = GlobalBase, "https://www.workbuddy.ai"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+authStatePath, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", err
	}
	loginHeaders(req, origin)
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", err
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("auth state: 响应缺少 state/authUrl")
	}
	return st.State, st.AuthURL, nil
}

// DeviceAuthPoll 设备授权第二步：完成返回凭证，未完成 done=false。
func (c *Client) DeviceAuthPoll(realm, state string) (*auth.Auth, bool, error) {
	base, origin := ChatBaseCN, "https://www.codebuddy.cn"
	if realm == "global" {
		base, origin = GlobalBase, "https://www.workbuddy.ai"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+authTokenPath+state, nil)
	if err != nil {
		return nil, false, err
	}
	loginHeaders(req, origin)
	data, err := c.doJSON(req)
	if err != nil {
		if ue, ok := err.(*Error); ok && (ue.Status == 0 || ue.Status >= 500) {
			return nil, false, err
		}
		return nil, false, nil // 登录未完成
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, false, nil
	}
	// 拿账号信息
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, base+loginAccountPath+state, nil)
	if err == nil {
		loginHeaders(req2, origin)
		req2.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if raw, err := c.doJSON(req2); err == nil {
			_ = json.Unmarshal(raw, &acct)
		}
	}
	if acct.UID == "" {
		return nil, false, fmt.Errorf("登录成功但未取到账号信息")
	}
	return &auth.Auth{
		UID: acct.UID, Nickname: acct.Nickname, EnterpriseID: acct.EnterpriseID,
		AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
		ExpiresAt: time.Now().Unix() + tok.ExpiresIn,
		Domain:    tok.Domain,
		Realm:     auth.ResolveRealm(realm, tok.Domain),
	}, true, nil
}

func loginHeaders(req *http.Request, origin string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2")
}

var hexChars = []byte("0123456789abcdef")

func randHex16() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", 32)
	}
	out := make([]byte, 32)
	for i, v := range b {
		out[i*2], out[i*2+1] = hexChars[v>>4], hexChars[v&15]
	}
	return string(out)
}
