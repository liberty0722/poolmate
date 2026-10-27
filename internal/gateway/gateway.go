// Package gateway 对外协议层：OpenAI Chat Completions、Anthropic Messages、
// 模型目录、健康检查与状态。共享中继核心：选号重试、SSE 重建、用量采集。
package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"poolmate/internal/config"
	"poolmate/internal/keys"
	"poolmate/internal/notify"
	"poolmate/internal/pool"
	"poolmate/internal/stats"
	"poolmate/internal/upstream"
)

// Server 网关服务。
type Server struct {
	cfg      *config.Config
	pool     *pool.Pool
	keys     *keys.Store
	stats    *stats.Store
	client   *upstream.Client
	notifier *notify.Notifier

	modelsMu sync.Mutex
	models   []upstream.Model
	modelsAt time.Time

	degradeMu    sync.Mutex
	degradeUntil time.Time // 内容拦截全局降级：最小提示词至次日零点
}

// New 创建网关。
func New(cfg *config.Config, p *pool.Pool, k *keys.Store, st *stats.Store, c *upstream.Client, n *notify.Notifier) *Server {
	return &Server{cfg: cfg, pool: p, keys: k, stats: st, client: c, notifier: n}
}

// Routes 返回 /v1 与健康/状态路由。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/messages", s.handleAnthropic)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /status", s.handleStatus)
	return mux
}

// ---------- 鉴权 ----------

func (s *Server) authGateway(r *http.Request, model string) (*keys.APIKey, string, bool) {
	c := s.cfg.Snapshot()
	token := ""
	for _, h := range []string{"Authorization", "x-api-key", "api-key"} {
		v := r.Header.Get(h)
		if v == "" {
			continue
		}
		token = strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
		break
	}
	if token == "" {
		return nil, "", c.Gateway.AllowAnonymous
	}
	if c.Gateway.APIKey != "" && token == c.Gateway.APIKey {
		return nil, "legacy", true // 旧单密钥，不限模型
	}
	k, err := s.keys.Validate(token, model)
	if err != nil {
		return nil, err.Error(), false
	}
	return k, "", true
}

func writeOpenAIError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": typ},
	})
}

// ---------- 请求体重写 ----------

// rewriteBody 出站前统一改写：强制流式、developer→system、提示词模式、思维链注入。
func (s *Server) rewriteBody(body map[string]any, degrade bool) {
	body["stream"] = true
	msgs, _ := body["messages"].([]any)
	mode := s.cfg.Snapshot().Prompt.Mode
	customText := s.cfg.Snapshot().Prompt.Text
	if customText == "" {
		customText = "You are a helpful assistant."
	}
	// developer 角色归一为 system
	for i, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "developer" {
			mm["role"] = "system"
			msgs[i] = mm
		}
	}
	switch {
	case degrade:
		// 全局降级：最小提示词替换全部 system
		out := []any{map[string]any{"role": "system", "content": customText}}
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && mm["role"] != "system" {
				out = append(out, m)
			}
		}
		msgs = out
	case mode == "custom":
		out := []any{map[string]any{"role": "system", "content": customText}}
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && mm["role"] != "system" {
				out = append(out, m)
			}
		}
		msgs = out
	case mode == "append":
		out := []any{}
		inserted := false
		for _, m := range msgs {
			out = append(out, m)
			if !inserted {
				if mm, ok := m.(map[string]any); !ok || mm["role"] != "system" {
					out = append(out[:len(out)-1], map[string]any{"role": "system", "content": customText}, m)
					inserted = true
				}
			}
		}
		if !inserted {
			out = append(out, map[string]any{"role": "system", "content": customText})
		}
		msgs = out
	}
	body["messages"] = msgs
	// 思维链注入：支持 reasoning_effort 的模型注入 thinking
	if _, ok := body["reasoning_effort"]; ok {
		body["thinking"] = map[string]any{"type": "enabled"}
	}
}

// prependSystemHint 内容拦截重试：在首条 system 前补一行（原文一字不改）。
func prependSystemHint(body map[string]any) {
	msgs, _ := body["messages"].([]any)
	hint := "You are a helpful assistant."
	for i, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "system" {
			if cs, ok := mm["content"].(string); ok {
				mm["content"] = hint + "\n" + cs
				msgs[i] = mm
				body["messages"] = msgs
				return
			}
		}
	}
	body["messages"] = append([]any{map[string]any{"role": "system", "content": hint}}, msgs...)
}

// ---------- 中继核心 ----------

// relayResult 一次请求的汇总（用量/账号/选号理由）。
type relayResult struct {
	Model        string
	Content      string
	Reasoning    string
	ToolCalls    []map[string]any
	FinishReason string
	Usage        pool.UsageObs
	UID, AName   string
	Realm        string
	FirstMs      int64
	TotalMs      int64
	Retries      int
	SelectReason string
}

// chunkSink 每个上游 SSE 数据帧（已解析 JSON）回调一次；返回错误中止流。
type chunkSink func(chunk map[string]any) error

var errRetryable = fmt.Errorf("retryable")

// relay 选中账号执行一次 chat 并把帧推给 sink。返回汇总或错误。
func (s *Server) relay(ctx context.Context, body map[string]any, sink chunkSink) (*relayResult, error) {
	cfg := s.cfg.Snapshot()
	model, _ := body["model"].(string)
	realm := ""
	if strings.HasPrefix(model, "cn:") {
		realm, model = "cn", strings.TrimPrefix(model, "cn:")
	} else if strings.HasPrefix(model, "global:") {
		realm, model = "global", strings.TrimPrefix(model, "global:")
	}
	// 模型别名：客户端请求别名时映射到真实上游模型（选号/冷却/转发均用真名）。
	if target, ok := cfg.Gateway.ModelAliases[model]; ok && target != "" && target != model {
		model = target
	}
	body["model"] = model
	// 每模型覆盖：注入默认思考档位与上下文长度档（客户端显式指定时尊重客户端）；钳制 max_tokens 上限。
	if ov, ok := cfg.Gateway.ModelOverrides[model]; ok {
		if ov.Effort != "" && ov.Effort != "auto" {
			if _, has := body["reasoning_effort"]; !has {
				body["reasoning_effort"] = ov.Effort
			}
		}
		if ov.ContextWindow > 0 {
			if _, has := body["context_window"]; !has {
				body["context_window"] = ov.ContextWindow
			}
		}
		if ov.MaxOutput > 0 {
			cur, _ := body["max_tokens"].(float64)
			if cur <= 0 || int(cur) > ov.MaxOutput {
				body["max_tokens"] = ov.MaxOutput
			}
		}
	}
	stickyKey := pool.StickyKey(body)
	convID := ""
	if md, ok := body["metadata"].(map[string]any); ok {
		for _, k := range []string{"conversation_id", "conversationId"} {
			if v, ok := md[k].(string); ok {
				convID = v
			}
		}
	}
	maxRetries := cfg.Pool.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	s.degradeMu.Lock()
	degraded := time.Now().Before(s.degradeUntil)
	s.degradeMu.Unlock()
	hintTried := false

	var lastErr error
	startAll := time.Now()
	for attempt := 0; attempt < maxRetries; attempt++ {
		acc, reason, err := s.pool.Select(stickyKey, model, realm)
		if err != nil {
			return nil, err
		}
		res := &relayResult{Model: model, Retries: attempt, SelectReason: reason}
		as := acc.Auth.Snapshot()
		res.UID, res.AName, res.Realm = as.UID, as.Nickname, as.Realm
		// token 快到期先刷新
		if acc.Auth.NeedsRefresh(5 * time.Minute) {
			if err := s.client.RefreshToken(acc.Auth); err != nil {
				s.pool.ReportFailure(acc, model, upstream.ErrAuth, err.Error())
				s.pool.Release(acc)
				lastErr = err
				continue
			}
		}
		out := deepCopy(body)
		s.rewriteBody(out, degraded)
		raw, _ := json.Marshal(out)
		rres, uerr := s.streamOnce(ctx, acc, model, raw, convID, sink, res)
		s.pool.Release(acc)
		if uerr == nil {
			s.pool.ReportSuccess(acc, model, res.Usage)
			res.TotalMs = time.Since(startAll).Milliseconds()
			return res, nil
		}
		lastErr = uerr
		// 内容拦截三层处理（仅未写过帧时才能重试）：
		// ① 原样 → ② 补提示行（原文不改）→ ③ 全局降级最小提示词至次日零点。
		if uerr.Kind == upstream.ErrContentBlocked {
			s.pool.ReportFailure(acc, model, uerr.Kind, uerr.Msg)
			if !rres && cfg.Prompt.Mode == "passthrough" {
				if !hintTried {
					hintTried = true
					prependSystemHint(body)
					continue
				}
				if !degraded {
					degraded = true
					s.degradeMu.Lock()
					now := time.Now()
					s.degradeUntil = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
					s.degradeMu.Unlock()
					continue
				}
			}
			return nil, uerr
		}
		s.pool.ReportFailure(acc, model, uerr.Kind, uerr.Msg)
	}
	return nil, lastErr
}

// streamOnce 对单账号执行一次流式调用。wroteAny 报告是否已向 sink 写过数据帧。
func (s *Server) streamOnce(ctx context.Context, acc *pool.Account, model string, raw []byte, convID string, sink chunkSink, res *relayResult) (wroteAny bool, uerr *upstream.Error) {
	start := time.Now()
	body, err := s.client.ChatStream(ctx, acc.Auth, raw, upstream.ChatMeta{ConversationID: convID})
	if err != nil {
		return false, err
	}
	defer body.Close()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		// 上游业务错误帧（流中返回 code!=0）
		if code, ok := chunk["code"].(float64); ok && code != 0 {
			msg, _ := chunk["msg"].(string)
			return wroteAny, &upstream.Error{Code: int(code), Msg: msg, Kind: upstream.Classify(200, int(code), msg)}
		}
		if first {
			res.FirstMs = time.Since(start).Milliseconds()
			first = false
		}
		wroteAny = true
		consumeChunk(res, chunk)
		if sink != nil {
			if err := sink(chunk); err != nil {
				return wroteAny, &upstream.Error{Kind: upstream.ErrNetwork, Msg: "client: " + err.Error()}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return wroteAny, &upstream.Error{Kind: upstream.ErrNetwork, Msg: err.Error()}
	}
	if !wroteAny {
		return false, &upstream.Error{Kind: upstream.ErrServer, Msg: "上游返回空流"}
	}
	return wroteAny, nil
}

// consumeChunk 从帧里聚合 content/reasoning/tool_calls/usage。
func consumeChunk(res *relayResult, chunk map[string]any) {
	if u, ok := chunk["usage"].(map[string]any); ok {
		res.Usage.PromptTokens = int64Of(u, "prompt_tokens")
		res.Usage.CompletionTokens = int64Of(u, "completion_tokens")
		res.Usage.CachedTokens = int64Of(u, "prompt_cache_hit_tokens")
		if c, ok := u["credit"].(float64); ok {
			res.Usage.Credit, res.Usage.HasCredit = c, true
		}
	}
	if c, ok := chunk["credit"].(float64); ok {
		res.Usage.Credit, res.Usage.HasCredit = c, true
	}
	choices, _ := chunk["choices"].([]any)
	for _, ch := range choices {
		cm, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
			res.FinishReason = fr
		}
		delta, _ := cm["delta"].(map[string]any)
		if delta == nil {
			delta, _ = cm["message"].(map[string]any)
		}
		if delta == nil {
			continue
		}
		if c, ok := delta["content"].(string); ok {
			res.Content += c
		}
		if r, ok := delta["reasoning_content"].(string); ok {
			res.Reasoning += r
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				if tm, ok := tc.(map[string]any); ok {
					res.ToolCalls = mergeToolCall(res.ToolCalls, tm)
				}
			}
		}
	}
}

// mergeToolCall 按 index 合并流式 tool_calls。
func mergeToolCall(calls []map[string]any, part map[string]any) []map[string]any {
	idx := 0
	if v, ok := part["index"].(float64); ok {
		idx = int(v)
	}
	for len(calls) <= idx {
		calls = append(calls, map[string]any{"id": "", "type": "function", "function": map[string]any{"name": "", "arguments": ""}})
	}
	c := calls[idx]
	if id, ok := part["id"].(string); ok && id != "" {
		c["id"] = id
	}
	if fn, ok := part["function"].(map[string]any); ok {
		f := c["function"].(map[string]any)
		if n, ok := fn["name"].(string); ok && n != "" {
			f["name"] = n
		}
		if a, ok := fn["arguments"].(string); ok {
			f["arguments"] = f["arguments"].(string) + a
		}
	}
	return calls
}

func int64Of(m map[string]any, k string) int64 {
	if v, ok := m[k].(float64); ok {
		return int64(v)
	}
	return 0
}

func deepCopy(m map[string]any) map[string]any {
	raw, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// ---------- OpenAI 端点 ----------

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&body); err != nil {
		writeOpenAIError(w, 400, "invalid json: "+err.Error(), "invalid_request_error")
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		writeOpenAIError(w, 400, "model is required", "invalid_request_error")
		return
	}
	apiKey, why, ok := s.authGateway(r, strings.TrimPrefix(strings.TrimPrefix(model, "cn:"), "global:"))
	if !ok {
		writeOpenAIError(w, 401, "鉴权失败："+why, "authentication_error")
		return
	}
	wantStream, _ := body["stream"].(bool)
	entry := stats.Entry{Model: model, Protocol: "openai", Stream: wantStream, Time: time.Now()}
	if apiKey != nil {
		entry.KeyID, entry.KeyName = apiKey.ID, apiKey.Name
	}
	flusher, _ := w.(http.Flusher)
	var sink chunkSink
	if wantStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		sink = func(chunk map[string]any) error {
			raw, _ := json.Marshal(chunk)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}
	}
	res, err := s.relay(r.Context(), body, sink)
	entry.UID, entry.AccountName = resUID(res), resAName(res)
	if err != nil {
		entry.Status = "error"
		if ue, ok := err.(*upstream.Error); ok {
			entry.ErrKind = kindName(ue.Kind)
			entry.ErrMsg = ue.Msg
		} else {
			entry.ErrKind = "no_account"
			entry.ErrMsg = err.Error()
		}
		entry.TotalMs = time.Since(entry.Time).Milliseconds()
		s.stats.Log(entry)
		if wantStream && res != nil && res.FirstMs > 0 {
			// 已写过帧：流内错误帧
			raw, _ := json.Marshal(map[string]any{"error": map[string]string{"message": err.Error(), "type": "upstream_error"}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
			return
		}
		if entry.ErrKind == "no_account" {
			writeOpenAIError(w, 503, "无可用账号："+err.Error(), "server_error")
		} else {
			writeOpenAIError(w, 502, "上游错误："+err.Error(), "upstream_error")
		}
		return
	}
	entry.Status = "ok"
	entry.PromptTokens = res.Usage.PromptTokens
	entry.CompTokens = res.Usage.CompletionTokens
	entry.CachedTokens = res.Usage.CachedTokens
	entry.Credits = res.Usage.Credit
	entry.FirstTokenMs = res.FirstMs
	entry.TotalMs = res.TotalMs
	entry.Retries = res.Retries
	entry.SelectReason = res.SelectReason
	s.stats.Log(entry)
	if apiKey != nil {
		s.keys.AddUsage(apiKey, res.Usage.Credit)
	}
	if wantStream {
		fmt.Fprintf(w, "data: [DONE]\n\n")
		return
	}
	// 非流式：聚合响应
	msg := map[string]any{"role": "assistant", "content": res.Content}
	if res.Reasoning != "" {
		msg["reasoning_content"] = res.Reasoning
	}
	if len(res.ToolCalls) > 0 {
		msg["tool_calls"] = res.ToolCalls
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-pm" + config.RandHex(8), "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": firstStr(res.FinishReason, "stop")}},
		"usage": map[string]any{
			"prompt_tokens": res.Usage.PromptTokens, "completion_tokens": res.Usage.CompletionTokens,
			"total_tokens": res.Usage.PromptTokens + res.Usage.CompletionTokens,
		},
	})
}

func resUID(r *relayResult) string {
	if r == nil {
		return ""
	}
	return r.UID
}

func resAName(r *relayResult) string {
	if r == nil {
		return ""
	}
	return r.AName
}

func firstStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func kindName(k upstream.ErrKind) string {
	switch k {
	case upstream.ErrRateLimit:
		return "rate_limit"
	case upstream.ErrNoCredit:
		return "no_credit"
	case upstream.ErrAuth:
		return "auth"
	case upstream.ErrNotFound:
		return "not_found"
	case upstream.ErrModelRateLimit:
		return "model_rate_limit"
	case upstream.ErrContentBlocked:
		return "content_blocked"
	case upstream.ErrServer:
		return "server"
	case upstream.ErrNetwork:
		return "network"
	}
	return "unknown"
}

// ---------- 模型目录 ----------

var fallbackModels = []upstream.Model{
	{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 1000000, MaxOutput: 64000, Rate: 0.69, Tools: true, Vision: true, Reasoning: []string{"low", "high", "max"}, DefaultEffort: "high", SupportedLengths: []int{300000, 600000, 1000000}},
	{ID: "glm-5.2", Name: "GLM-5.2", ContextWindow: 1000000, MaxOutput: 128000, Rate: 0.79, Tools: true, Reasoning: []string{"high", "xhigh"}, DefaultEffort: "high", SupportedLengths: []int{300000, 600000, 1000000}},
	{ID: "kimi-k3.1-pro", Name: "Kimi-K3.1-Pro", ContextWindow: 1000000, MaxOutput: 32000, Rate: 0.97, Tools: true, Vision: true, Reasoning: []string{"low", "high", "max"}, DefaultEffort: "high", SupportedLengths: []int{300000, 600000, 1000000}},
	{ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash", ContextWindow: 1000000, MaxOutput: 128000, Rate: 0.11, Tools: true, Vision: true, Reasoning: []string{"low", "high", "max"}, DefaultEffort: "high", CanDisableThinking: true, SupportedLengths: []int{300000, 600000, 1000000}},
	{ID: "hy4-preview", Name: "Hy4 preview", ContextWindow: 960000, MaxOutput: 64000, Rate: 0.29, Tools: true, Vision: true, Reasoning: []string{"high"}, DefaultEffort: "high", SupportedLengths: []int{300000, 600000, 960000}},
	{ID: "minimax-m3", Name: "MiniMax-M3", ContextWindow: 512000, MaxOutput: 524000, Tools: true, Vision: true, Reasoning: []string{"medium"}, DefaultEffort: "medium"},
}

// Catalog 模型目录：缓存 10 分钟，失败用静态兜底。
func (s *Server) Catalog() []upstream.Model {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	if len(s.models) > 0 && time.Since(s.modelsAt) < 10*time.Minute {
		return s.models
	}
	for _, snap := range s.pool.List() {
		if snap.Status != "ready" && snap.Status != "budget" {
			continue
		}
		acc := s.pool.Get(snap.UID)
		if acc == nil || acc.Auth == nil {
			continue
		}
		models, err := s.client.Models(acc.Auth)
		if err == nil && len(models) > 0 {
			s.models, s.modelsAt = models, time.Now()
			return s.models
		}
	}
	if len(s.models) > 0 {
		return s.models
	}
	return fallbackModels
}

// InvalidateCatalog 强制刷新目录缓存。
func (s *Server) InvalidateCatalog() {
	s.modelsMu.Lock()
	s.modelsAt = time.Time{}
	s.modelsMu.Unlock()
}

// MergedCatalog 应用每模型覆盖（上下文/最大输出）后的目录，用于 /v1/models 与面板展示。
func (s *Server) MergedCatalog() []upstream.Model {
	cfg := s.cfg.Snapshot()
	ms := s.Catalog()
	for i, m := range ms {
		if ov, ok := cfg.Gateway.ModelOverrides[m.ID]; ok {
			if ov.ContextWindow > 0 {
				ms[i].ContextWindow = ov.ContextWindow
			}
			if ov.MaxOutput > 0 {
				ms[i].MaxOutput = ov.MaxOutput
			}
		}
	}
	return ms
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Snapshot()
	// 鉴权：模型目录也不应裸奔（会暴露可用模型与倍率信息）。
	token := ""
	for _, h := range []string{"Authorization", "x-api-key", "api-key"} {
		if v := r.Header.Get(h); v != "" {
			token = strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
			break
		}
	}
	if token == "" && !cfg.Gateway.AllowAnonymous {
		writeOpenAIError(w, http.StatusUnauthorized, "missing api key", "authentication_error")
		return
	}
	if token != "" {
		legacyOK := cfg.Gateway.APIKey != "" && token == cfg.Gateway.APIKey
		if !legacyOK && s.keys.Ping(token) != nil {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid api key", "authentication_error")
			return
		}
	}
	var list []any
	for _, m := range s.MergedCatalog() {
		if modelHidden(cfg.Gateway.ModelHide, m.ID) {
			continue
		}
		name := m.ID
		if cfg.Gateway.CreditInName && m.Rate > 0 {
			name = fmt.Sprintf("%s(x%g)", m.ID, m.Rate)
		}
		display := m.Name
		if cfg.Gateway.ModelNaming == "full" || cfg.Gateway.ModelNaming == "both" {
			display = firstStr(m.Name, m.ID)
		}
		list = append(list, map[string]any{
			"id": name, "object": "model", "created": time.Now().Unix(), "owned_by": "poolmate",
			"display_name": display, "context_window": m.ContextWindow, "max_output": m.MaxOutput,
			"vision": m.Vision, "tools": m.Tools, "reasoning_levels": m.Reasoning, "credit_rate": m.Rate,
			"default_effort": m.DefaultEffort, "can_disable_thinking": m.CanDisableThinking,
			"supported_lengths": m.SupportedLengths, "description": m.Description,
		})
	}
	// 别名条目：以别名 id 透出，元数据继承目标模型（若目标在目录中）。
	for alias, target := range cfg.Gateway.ModelAliases {
		if alias == "" || modelHidden(cfg.Gateway.ModelHide, alias) {
			continue
		}
		entry := map[string]any{
			"id": alias, "object": "model", "created": time.Now().Unix(), "owned_by": "poolmate",
			"display_name": alias, "alias_of": target,
		}
		for _, m := range s.Catalog() {
			if m.ID == target {
				entry["display_name"] = fmt.Sprintf("%s → %s", alias, firstStr(m.Name, m.ID))
				entry["context_window"] = m.ContextWindow
				entry["max_output"] = m.MaxOutput
				entry["vision"] = m.Vision
				entry["tools"] = m.Tools
				entry["reasoning_levels"] = m.Reasoning
				entry["credit_rate"] = m.Rate
				entry["default_effort"] = m.DefaultEffort
				break
			}
		}
		list = append(list, entry)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": list})
}

// modelHidden 判断模型是否被隐藏（尾部 * 通配，与密钥白名单同语义）。
func modelHidden(patterns []string, model string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == "*" || p == model {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(model, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// ---------- 健康与状态 ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ready, total, _, _ := s.pool.Summary()
	status := 200
	if ready == 0 {
		status = 503
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"healthy": ready, "total": total, "service": "poolmate", "version": Version,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ready, total, balance, expiring := s.pool.Summary()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready": ready, "total": total, "balance": balance, "expiring_7d": expiring,
		"accounts": s.pool.List(), "version": Version,
	})
}

// Version 版本号（main 注入）。
var Version = "0.1.0"
