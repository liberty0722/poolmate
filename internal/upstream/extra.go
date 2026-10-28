package upstream

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"poolmate/internal/auth"
)

// ---------- 代理 ----------

var proxyURL atomic.Value // *url.URL（nil 表示直连）

// SetProxy 运行时切换出站代理；空串表示直连。支持 http/https/socks5。
func SetProxy(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		proxyURL.Store((*url.URL)(nil))
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("代理地址无效: %q", raw)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("不支持的代理协议: %s", u.Scheme)
	}
	proxyURL.Store(u)
	return nil
}

func proxyFunc(_ *http.Request) (*url.URL, error) {
	if v, ok := proxyURL.Load().(*url.URL); ok {
		return v, nil
	}
	return nil, nil
}

// ---------- 用量提醒 / 付费类型 ----------

// DosageNotice 上游用量提醒。
type DosageNotice struct {
	Code int    `json:"code"`
	Zh   string `json:"zh"`
	En   string `json:"en"`
	URL  string `json:"url"`
}

// DosageNotify 查询用量提醒（Code!=0 表示上游认为额度告急/需要提示）。
func (c *Client) DosageNotify(a *auth.Auth) (*DosageNotice, error) {
	data, err := c.meterJSON(a, []string{dosageNotifyPath}, http.MethodPost, map[string]any{})
	if err != nil {
		return nil, err
	}
	var w struct {
		Code int    `json:"dosageNotifyCode"`
		Zh   string `json:"dosageNotifyZh"`
		En   string `json:"dosageNotifyEn"`
		URL  string `json:"skipUrl"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("dosage parse: %w", err)
	}
	return &DosageNotice{Code: w.Code, Zh: w.Zh, En: w.En, URL: w.URL}, nil
}

// PaymentType 查询付费类型（free / pro / enterprise ...）。
func (c *Client) PaymentType(a *auth.Auth) (string, error) {
	data, err := c.meterJSON(a, []string{paymentTypePath}, http.MethodPost, map[string]any{})
	if err != nil {
		return "", err
	}
	var w struct {
		PaymentType string `json:"paymentType"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return "", fmt.Errorf("payment parse: %w", err)
	}
	return w.PaymentType, nil
}

// ---------- 企业用量 ----------

func anyNum(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case float64:
			return t, true
		case json.Number:
			if f, err := t.Float64(); err == nil {
				return f, true
			}
		case string:
			var f float64
			if _, err := fmt.Sscanf(strings.TrimSpace(t), "%g", &f); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func anyTime(m map[string]any, keys ...string) (time.Time, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			s := strings.TrimSpace(t)
			if s == "" {
				continue
			}
			for _, lay := range []string{packageEndLayout, time.RFC3339, "2006-01-02"} {
				if tm, err := time.ParseInLocation(lay, s, time.Local); err == nil {
					return tm, true
				}
			}
		case float64:
			if t > 1e12 {
				return time.UnixMilli(int64(t)), true
			}
			if t > 1e9 {
				return time.Unix(int64(t), 0), true
			}
		}
	}
	return time.Time{}, false
}

// EnterpriseUsage 企业成员用量（企业 ID 经 X-Enterprise-Id / X-Tenant-Id 传递），
// 合成为一个「企业版」积分包。limit=-1 视为不限量。
func (c *Client) EnterpriseUsage(a *auth.Auth) (*Package, error) {
	s := a.Snapshot()
	if s.EnterpriseID == "" {
		return nil, fmt.Errorf("非企业账号")
	}
	data, err := c.enterpriseCall(a)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("enterprise usage parse: %w", err)
	}
	// 响应可能多包一层 data
	if inner, ok := m["data"].(map[string]any); ok {
		m = inner
	}
	limit, hasLimit := anyNum(m, "limit_num", "limitNum")
	used, _ := anyNum(m, "used_num", "usedNum", "credit")
	if !hasLimit && used == 0 {
		return nil, fmt.Errorf("企业用量响应无可识别字段")
	}
	p := &Package{Name: "企业版", Used: int64(used)}
	if limit < 0 {
		p.Size = math.MaxInt32
		p.Remain = math.MaxInt32
	} else {
		p.Size = int64(limit)
		rem := limit - used
		if rem < 0 {
			rem = 0
		}
		p.Remain = int64(rem)
	}
	if t, ok := anyTime(m, "cycle_end_time", "cycleEndTime", "cycle_reset_time", "cycleResetTime"); ok {
		p.CycleEnd, p.HasCycle = t, true
	}
	return p, nil
}

func (c *Client) enterpriseCall(a *auth.Auth) (json.RawMessage, error) {
	s := a.Snapshot()
	// meterJSON 使用 billingHeaders；这里额外补 X-Tenant-Id，通过包装 headers 实现。
	return c.meterJSONWith(a, []string{enterpriseUsagePth}, http.MethodPost, map[string]any{},
		map[string]string{"X-Enterprise-Id": s.EnterpriseID, "X-Tenant-Id": s.EnterpriseID})
}
