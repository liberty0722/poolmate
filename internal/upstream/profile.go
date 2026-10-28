package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"poolmate/internal/auth"
)

// Profile 账号基本信息。
type Profile struct {
	UID          string
	Nickname     string
	EnterpriseID string
}

// ProfileByToken 凭 access token 查询账号信息（/console/login/account）。
func (c *Client) ProfileByToken(realm, accessToken string) (*Profile, error) {
	base, origin := ChatBaseCN, BillingBaseCN
	if realm == "global" {
		base, origin = GlobalBase, GlobalBase
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+consoleAccountPath, nil)
	if err != nil {
		return nil, err
	}
	loginHeaders(req, origin)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	raw, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal(raw, &acct); err != nil || acct.UID == "" {
		return nil, fmt.Errorf("账号信息响应无 uid")
	}
	return &Profile{UID: acct.UID, Nickname: acct.Nickname, EnterpriseID: acct.EnterpriseID}, nil
}

// WakeUp 发起一次最小开销的真实对话请求（max_tokens=1），
// 让套餐的额度重置周期尽早起算。model 为空时自动选倍率最低的模型。返回实际使用的模型。
func (c *Client) WakeUp(a *auth.Auth, model string) (string, error) {
	if model == "" {
		ms, err := c.Models(a)
		if err != nil {
			return "", fmt.Errorf("取模型目录失败: %w", err)
		}
		best := -1
		for i, m := range ms {
			if m.ID == "" {
				continue
			}
			if best < 0 || m.Rate < ms[best].Rate {
				best = i
			}
		}
		if best < 0 {
			return "", fmt.Errorf("模型目录为空")
		}
		model = ms[best].ID
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rc, uerr := c.ChatStream(ctx, a, body, ChatMeta{})
	if uerr != nil {
		return model, uerr
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 256*1024))
	return model, nil
}
