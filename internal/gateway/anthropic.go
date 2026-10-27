// Anthropic Messages 协议适配：/v1/messages 与 /v1/messages/count_tokens。
// 请求：system→system 消息、content 块→OpenAI parts、tool_use/tool_result↔tool_calls、
// thinking.budget_tokens→reasoning_effort 分档。响应：thinking/text/tool_use 块与完整流式事件。
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"poolmate/internal/config"
	"poolmate/internal/stats"
	"poolmate/internal/upstream"
)

// ---------- 请求转换 ----------

func anthropicToOpenAI(body map[string]any) (map[string]any, error) {
	out := map[string]any{}
	model, _ := body["model"].(string)
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}
	out["model"] = model
	if mt, ok := body["max_tokens"].(float64); ok {
		out["max_tokens"] = int(mt)
	} else {
		return nil, fmt.Errorf("max_tokens is required")
	}
	var msgs []any
	// system：字符串或块数组 → 前置 system 消息（兼收 messages 里混入的 system/developer 角色）
	var sysTexts []string
	addSys := func(v any) {
		switch s := v.(type) {
		case string:
			if s != "" {
				sysTexts = append(sysTexts, s)
			}
		case []any:
			for _, b := range s {
				if bm, ok := b.(map[string]any); ok {
					if t, ok := bm["text"].(string); ok && t != "" {
						sysTexts = append(sysTexts, t)
					}
				}
			}
		}
	}
	addSys(body["system"])
	for _, m := range arrOf(body, "messages") {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		switch role {
		case "system", "developer":
			addSys(mm["content"])
		case "user":
			msgs = append(msgs, convertUserMsg(mm))
		case "assistant":
			msgs = append(msgs, convertAssistantMsg(mm))
		default:
			return nil, fmt.Errorf("unsupported role %q", role)
		}
	}
	if len(sysTexts) > 0 {
		msgs = append([]any{map[string]any{"role": "system", "content": strings.Join(sysTexts, "\n")}}, msgs...)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("messages is required")
	}
	out["messages"] = msgs
	// tools
	var tools []any
	for _, t := range arrOf(body, "tools") {
		if tm, ok := t.(map[string]any); ok {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": tm["name"], "description": tm["description"],
					"parameters": firstNonNil(tm["input_schema"], map[string]any{"type": "object"}),
				},
			})
		}
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if tc, ok := body["tool_choice"].(map[string]any); ok {
		switch tc["type"] {
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc["name"]}}
		default:
			out["tool_choice"] = "auto"
		}
	}
	if ss := arrOf(body, "stop_sequences"); len(ss) > 0 {
		out["stop"] = ss
	}
	// thinking → reasoning_effort 分档
	if th, ok := body["thinking"].(map[string]any); ok && th["type"] == "enabled" {
		effort := "low"
		if b, ok := th["budget_tokens"].(float64); ok {
			switch {
			case b >= 16000:
				effort = "high"
			case b >= 8000:
				effort = "medium"
			}
		}
		out["reasoning_effort"] = effort
	}
	// metadata.user_id 作为粘性键
	if md, ok := body["metadata"].(map[string]any); ok {
		out["metadata"] = md
	}
	if st, ok := body["stream"].(bool); ok {
		out["stream"] = st
	}
	return out, nil
}

func convertUserMsg(mm map[string]any) map[string]any {
	content := mm["content"]
	parts, ok := content.([]any)
	if !ok {
		return map[string]any{"role": "user", "content": content}
	}
	var out []any
	var toolResults []map[string]any
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pm["type"] {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": pm["text"]})
		case "image":
			if src, ok := pm["source"].(map[string]any); ok {
				if src["type"] == "base64" {
					out = append(out, map[string]any{"type": "image_url",
						"image_url": map[string]any{"url": fmt.Sprintf("data:%v;base64,%v", src["media_type"], src["data"])}})
				} else if u, ok := src["url"].(string); ok {
					out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
				}
			}
		case "tool_result":
			toolResults = append(toolResults, map[string]any{
				"role": "tool", "tool_call_id": pm["tool_use_id"], "content": stringify(pm["content"]),
			})
		}
	}
	if len(toolResults) > 0 && len(out) == 0 {
		return toolResults[0] // 单 tool_result 消息
	}
	return map[string]any{"role": "user", "content": out}
}

func convertAssistantMsg(mm map[string]any) map[string]any {
	content := mm["content"]
	parts, ok := content.([]any)
	if !ok {
		return map[string]any{"role": "assistant", "content": content}
	}
	var text strings.Builder
	var reasoning string
	var toolCalls []map[string]any
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pm["type"] {
		case "text":
			text.WriteString(stringify(pm["text"]))
		case "thinking":
			reasoning += stringify(pm["thinking"])
		case "tool_use":
			args, _ := json.Marshal(pm["input"])
			toolCalls = append(toolCalls, map[string]any{
				"id": pm["id"], "type": "function",
				"function": map[string]any{"name": pm["name"], "arguments": string(args)},
			})
		}
	}
	out := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning != "" {
		out["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		out["tool_calls"] = toolCalls
	}
	return out
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, x := range t {
			if xm, ok := x.(map[string]any); ok {
				sb.WriteString(stringify(xm["text"]))
			}
		}
		return sb.String()
	}
	return ""
}

func arrOf(m map[string]any, k string) []any {
	if v, ok := m[k].([]any); ok {
		return v
	}
	return nil
}

func firstNonNil(a, b any) any {
	if a != nil {
		return a
	}
	return b
}

// ---------- 响应转换 ----------

func stopReason(finish string, hasTools bool) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	if hasTools {
		return "tool_use"
	}
	return "end_turn"
}

func anthropicMessage(res *relayResult, model string) map[string]any {
	var blocks []any
	if res.Reasoning != "" {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": res.Reasoning, "signature": ""})
	}
	if res.Content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": res.Content})
	}
	for _, tc := range res.ToolCalls {
		fn := tc["function"].(map[string]any)
		var input any = map[string]any{}
		_ = json.Unmarshal([]byte(fn["arguments"].(string)), &input)
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": tc["id"], "name": fn["name"], "input": input,
		})
	}
	return map[string]any{
		"id": "msg_pm" + config.RandHex(8), "type": "message", "role": "assistant",
		"model": model, "content": blocks, "stop_reason": stopReason(res.FinishReason, len(res.ToolCalls) > 0),
		"usage": map[string]any{
			"input_tokens": res.Usage.PromptTokens, "output_tokens": res.Usage.CompletionTokens,
			"cache_read_input_tokens": res.Usage.CachedTokens,
		},
	}
}

// ---------- 流式转换 ----------

// anthropicStream 把 OpenAI 帧流转为 Anthropic 事件流。
type anthropicStream struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	msgID    string
	model    string
	started  bool
	blockIdx int
	openKind string // text / thinking / tool_use
	toolID   string
	toolName string
}

func (as *anthropicStream) emit(event string, data map[string]any) error {
	raw, _ := json.Marshal(data)
	if _, err := fmt.Fprintf(as.w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if as.flusher != nil {
		as.flusher.Flush()
	}
	return nil
}

func (as *anthropicStream) start() error {
	if as.started {
		return nil
	}
	as.started = true
	return as.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": as.msgID, "type": "message", "role": "assistant", "model": as.model,
			"content": []any{}, "stop_reason": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (as *anthropicStream) closeBlock() error {
	if as.openKind == "" {
		return nil
	}
	if err := as.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": as.blockIdx}); err != nil {
		return err
	}
	as.blockIdx++
	as.openKind = ""
	return nil
}

func (as *anthropicStream) openBlock(kind string) error {
	if as.openKind == kind {
		return nil
	}
	if err := as.closeBlock(); err != nil {
		return err
	}
	var block map[string]any
	switch kind {
	case "text":
		block = map[string]any{"type": "text", "text": ""}
	case "thinking":
		block = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	case "tool_use":
		block = map[string]any{"type": "tool_use", "id": as.toolID, "name": as.toolName, "input": map[string]any{}}
	}
	if err := as.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": as.blockIdx, "content_block": block,
	}); err != nil {
		return err
	}
	as.openKind = kind
	return nil
}

func (as *anthropicStream) delta(kind, text string) error {
	if err := as.openBlock(kind); err != nil {
		return err
	}
	dtype := "text_delta"
	if kind == "thinking" {
		dtype = "thinking_delta"
	} else if kind == "tool_use" {
		dtype = "input_json_delta"
		return as.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": as.blockIdx,
			"delta": map[string]any{"type": dtype, "partial_json": text},
		})
	}
	return as.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": as.blockIdx,
		"delta": map[string]any{"type": dtype, "text": text},
	})
}

// onChunk 处理一个 OpenAI 帧。
func (as *anthropicStream) onChunk(chunk map[string]any) error {
	if err := as.start(); err != nil {
		return err
	}
	choices, _ := chunk["choices"].([]any)
	for _, ch := range choices {
		cm, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		delta, _ := cm["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if r, ok := delta["reasoning_content"].(string); ok && r != "" {
			if err := as.delta("thinking", r); err != nil {
				return err
			}
		}
		if c, ok := delta["content"].(string); ok && c != "" {
			if err := as.delta("text", c); err != nil {
				return err
			}
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tm, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				if id, ok := tm["id"].(string); ok && id != "" {
					as.toolID = id
					if fn, ok := tm["function"].(map[string]any); ok {
						if n, ok := fn["name"].(string); ok && n != "" {
							as.toolName = n
						}
					}
					if err := as.openBlock("tool_use"); err != nil {
						return err
					}
				}
				if fn, ok := tm["function"].(map[string]any); ok {
					if a, ok := fn["arguments"].(string); ok && a != "" {
						if err := as.delta("tool_use", a); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

func (as *anthropicStream) finish(res *relayResult) error {
	if err := as.start(); err != nil {
		return err
	}
	if err := as.closeBlock(); err != nil {
		return err
	}
	if err := as.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(res.FinishReason, len(res.ToolCalls) > 0), "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens": res.Usage.PromptTokens, "output_tokens": res.Usage.CompletionTokens,
			"cache_read_input_tokens": res.Usage.CachedTokens,
		},
	}); err != nil {
		return err
	}
	return as.emit("message_stop", map[string]any{"type": "message_stop"})
}

// ---------- 端点 ----------

func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	var raw map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&raw); err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "invalid json: "+err.Error())
		return
	}
	body, err := anthropicToOpenAI(raw)
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	model := body["model"].(string)
	apiKey, why, ok := s.authGateway(r, model)
	if !ok {
		writeAnthropicError(w, 401, "authentication_error", why)
		return
	}
	wantStream, _ := raw["stream"].(bool)
	entry := stats.Entry{Model: model, Protocol: "anthropic", Stream: wantStream, Time: time.Now()}
	if apiKey != nil {
		entry.KeyID, entry.KeyName = apiKey.ID, apiKey.Name
	}
	var sink chunkSink
	var as *anthropicStream
	if wantStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		as = &anthropicStream{w: w, flusher: flusher, msgID: "msg_pm" + config.RandHex(8), model: model}
		sink = as.onChunk
	}
	res, rerr := s.relay(r.Context(), body, sink)
	entry.UID, entry.AccountName = resUID(res), resAName(res)
	if rerr != nil {
		entry.Status = "error"
		if ue, ok := rerr.(*upstream.Error); ok {
			entry.ErrKind, entry.ErrMsg = kindName(ue.Kind), ue.Msg
		} else {
			entry.ErrKind, entry.ErrMsg = "no_account", rerr.Error()
		}
		entry.TotalMs = time.Since(entry.Time).Milliseconds()
		s.stats.Log(entry)
		if wantStream && as.started {
			_ = as.emit("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": rerr.Error()}})
			return
		}
		status := 502
		if entry.ErrKind == "no_account" {
			status = 503
		}
		writeAnthropicError(w, status, "api_error", rerr.Error())
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
		_ = as.finish(res)
		return
	}
	_ = json.NewEncoder(w).Encode(anthropicMessage(res, model))
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error", "error": map[string]string{"type": typ, "message": msg},
	})
}

// handleCountTokens 启发式估算（约 3.5 字符/token + 图片 1600 定额），不调上游。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	var raw map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&raw); err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	var chars, images int64
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			chars += int64(len([]rune(t)))
		case map[string]any:
			if t["type"] == "image" {
				images++
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
	walk(raw["system"])
	walk(raw["messages"])
	tokens := int64(float64(chars)/3.5) + images*1600
	_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": tokens})
}
