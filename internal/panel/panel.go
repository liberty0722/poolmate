// Package panel Web 管理面板：内嵌单页面前端 + 管理 API。
// 全部 API 需要 X-Admin-Key 头（或 ?key= 查询参数）鉴权。
package panel

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"poolmate/internal/auth"
	"poolmate/internal/backup"
	"poolmate/internal/config"
	"poolmate/internal/gateway"
	"poolmate/internal/keys"
	"poolmate/internal/notify"
	"poolmate/internal/pool"
	"poolmate/internal/scheduler"
	"poolmate/internal/stats"
	"poolmate/internal/upstream"
)

//go:embed web
var webFS embed.FS

// Deps 面板依赖。
type Deps struct {
	Cfg         *config.Config
	Pool        *pool.Pool
	Keys        *keys.Store
	Stats       *stats.Store
	Client      *upstream.Client
	Notifier    *notify.Notifier
	Scheduler   *scheduler.Scheduler
	Gateway     *gateway.Server
	AuthDir     string
	StateFile   string
	ReloadAuths func()
	// VaultPassword 主密码（仅内存，不落盘）；空 = 机器因子。
	VaultPassword func() string
	SetVaultPass  func(string)
}

// Server 面板服务。
type Server struct {
	d Deps
	mu sync.Mutex
	loginStates map[string]loginState
}

type loginState struct {
	Realm string
	At    time.Time
}

func New(d Deps) *Server {
	return &Server{d: d, loginStates: map[string]loginState{}}
}

// Handler 返回面板路由（/api/* 与 / 静态）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// 静态前端
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	// API
	mux.HandleFunc("GET /api/overview", s.guard(s.overview))
	mux.HandleFunc("GET /api/accounts", s.guard(s.accounts))
	mux.HandleFunc("POST /api/accounts/{uid}/action", s.guard(s.accountAction))
	mux.HandleFunc("POST /api/accounts/batch", s.guard(s.accountBatch))
	mux.HandleFunc("POST /api/accounts/import", s.guard(s.importAccounts))
	mux.HandleFunc("POST /api/accounts/export", s.guard(s.exportAccounts))
	mux.HandleFunc("POST /api/login/start", s.guard(s.loginStart))
	mux.HandleFunc("POST /api/login/poll", s.guard(s.loginPoll))
	mux.HandleFunc("GET /api/keys", s.guard(s.listKeys))
	mux.HandleFunc("POST /api/keys", s.guard(s.createKey))
	mux.HandleFunc("PUT /api/keys/{id}", s.guard(s.updateKey))
	mux.HandleFunc("DELETE /api/keys/{id}", s.guard(s.deleteKey))
	mux.HandleFunc("POST /api/keys/{id}/rotate", s.guard(s.rotateKey))
	mux.HandleFunc("GET /api/logs", s.guard(s.logs))
	mux.HandleFunc("GET /api/usage", s.guard(s.usage))
	mux.HandleFunc("GET /api/models", s.guard(s.models))
	mux.HandleFunc("PUT /api/models/{id}/override", s.guard(s.modelOverrideSave))
	mux.HandleFunc("POST /api/models/refresh", s.guard(s.modelsRefresh))
	mux.HandleFunc("GET /api/tasks", s.guard(s.tasks))
	mux.HandleFunc("POST /api/tasks/{name}/run", s.guard(s.runTask))
	mux.HandleFunc("GET /api/tasks/history", s.guard(s.taskHistory))
	mux.HandleFunc("GET /api/settings", s.guard(s.getSettings))
	mux.HandleFunc("PUT /api/settings", s.guard(s.putSettings))
	mux.HandleFunc("POST /api/notify/test", s.guard(s.notifyTest))
	mux.HandleFunc("GET /api/notify/recent", s.guard(s.notifyRecent))
	mux.HandleFunc("POST /api/diagnose", s.guard(s.diagnose))
	mux.HandleFunc("GET /api/backups", s.guard(s.listBackups))
	mux.HandleFunc("POST /api/backups/now", s.guard(s.backupNow))
	mux.HandleFunc("POST /api/vault/password", s.guard(s.setVaultPassword))
	return mux
}

func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Admin-Key")
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		if s.d.Cfg.Snapshot().AdminKey == "" || key != s.d.Cfg.Snapshot().AdminKey {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ---------- 概览 ----------

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ready, total, balance, expiring := s.d.Pool.Summary()
	today := s.d.Stats.Today()
	// 预计浪费：7 天内到期 − 按当日速度 7 天可用量
	var waste int64
	for _, a := range s.d.Pool.List() {
		daily := a.DailyCredit
		if daily <= 0 {
			daily = 50
		}
		if over := a.Expiring7d - int64(daily*7); over > 0 {
			waste += over
		}
	}
	var attention []map[string]string
	for _, a := range s.d.Pool.List() {
		switch a.Status {
		case "disabled":
			attention = append(attention, map[string]string{"level": "critical", "text": fmt.Sprintf("账号「%s」已禁用：%s", a.Name, a.StatusWhy)})
		case "breaker":
			attention = append(attention, map[string]string{"level": "warn", "text": fmt.Sprintf("账号「%s」熔断中：%s", a.Name, a.StatusWhy)})
		case "cooling":
			attention = append(attention, map[string]string{"level": "info", "text": fmt.Sprintf("账号「%s」冷却中：%s", a.Name, a.StatusWhy)})
		}
		if days := a.ExpiryDays; days < 7 {
			attention = append(attention, map[string]string{"level": "warn", "text": fmt.Sprintf("账号「%s」凭证 %.1f 天后到期", a.Name, days)})
		}
	}
	writeJSON(w, map[string]any{
		"accounts_ready": ready, "accounts_total": total,
		"balance": balance, "expiring_7d": expiring, "waste_forecast": waste,
		"today": today, "last24h": s.d.Stats.Last24h(),
		"attention": attention, "recent": s.d.Stats.Query(stats.Filter{Limit: 10}),
		"version": gateway.Version,
	})
}

// ---------- 账号 ----------

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"accounts": s.d.Pool.List()})
}

func (s *Server) accountAction(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var req struct {
		Action string `json:"action"` // refresh / checkin / pause / resume / revive / delete
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	acc := s.d.Pool.Get(uid)
	if acc == nil || acc.Auth == nil {
		writeErr(w, 404, "账号不存在")
		return
	}
	switch req.Action {
	case "refresh":
		remain, packs, err := s.d.Client.Balance(acc.Auth)
		s.d.Pool.SetBalance(acc, remain, packs, err)
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
	case "checkin":
		if err := s.d.Client.DailyCheckin(acc.Auth); err != nil && !upstream.IsAlreadyCheckin(err) {
			writeErr(w, 502, err.Error())
			return
		}
	case "pause":
		s.d.Pool.Pause(uid, req.Reason)
	case "resume":
		s.d.Pool.Resume(uid)
	case "revive":
		s.d.Pool.Revive(uid)
	case "delete":
		if err := auth.Delete(s.d.AuthDir, uid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		s.d.Pool.Remove(uid)
		s.d.ReloadAuths()
	default:
		writeErr(w, 400, "未知动作")
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) accountBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string   `json:"action"`
		UIDs   []string `json:"uids"`
		Reason string   `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var ok, failed int
	var errs []string
	for _, uid := range req.UIDs {
		acc := s.d.Pool.Get(uid)
		if acc == nil || acc.Auth == nil {
			failed++
			continue
		}
		var err error
		switch req.Action {
		case "refresh":
			var remain int64
			var packs []upstream.Package
			remain, packs, err = s.d.Client.Balance(acc.Auth)
			s.d.Pool.SetBalance(acc, remain, packs, err)
		case "checkin":
			err = s.d.Client.DailyCheckin(acc.Auth)
			if upstream.IsAlreadyCheckin(err) {
				err = nil
			}
		case "pause":
			s.d.Pool.Pause(uid, req.Reason)
		case "resume":
			s.d.Pool.Resume(uid)
		case "revive":
			s.d.Pool.Revive(uid)
		case "delete":
			err = auth.Delete(s.d.AuthDir, uid)
			if err == nil {
				s.d.Pool.Remove(uid)
			}
		}
		if err != nil {
			failed++
			errs = append(errs, uid[:8]+": "+err.Error())
		} else {
			ok++
		}
	}
	if req.Action == "delete" {
		s.d.ReloadAuths()
	}
	writeJSON(w, map[string]any{"ok": ok, "failed": failed, "errors": errs})
}

func (s *Server) importAccounts(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	password := r.URL.Query().Get("password")
	auths, skipped, err := backup.ImportBundle(raw, password)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c := s.d.Cfg.Snapshot()
	var imported []string
	for uid, a := range auths {
		// 池中已有更新的同一账号则跳过
		if old := s.d.Pool.Get(uid); old != nil && old.Auth != nil {
			if old.Auth.Snapshot().ExpiresAt >= a.Snapshot().ExpiresAt {
				skipped = append(skipped, a.DisplayName()+"（池中凭证更新，跳过）")
				continue
			}
		}
		if err := auth.Save(s.d.AuthDir, a, c.Security.VaultEnabled, s.d.VaultPassword()); err != nil {
			skipped = append(skipped, uid[:8]+" 保存失败："+err.Error())
			continue
		}
		imported = append(imported, a.DisplayName())
	}
	s.d.ReloadAuths()
	writeJSON(w, map[string]any{"imported": imported, "skipped": skipped})
}

func (s *Server) exportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs     []string `json:"uids"`
		Password string   `json:"password"` // 非空则加密导出
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	auths := map[string]*auth.Auth{}
	for _, snap := range s.d.Pool.List() {
		if len(req.UIDs) > 0 {
			found := false
			for _, u := range req.UIDs {
				if u == snap.UID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if acc := s.d.Pool.Get(snap.UID); acc != nil && acc.Auth != nil {
			auths[snap.UID] = acc.Auth
		}
	}
	raw, err := backup.ExportBundle(auths, req.Password)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=poolmate-backup-%s.json", time.Now().Format("20060102-150405")))
	_, _ = w.Write(raw)
}

// ---------- 登录（设备授权） ----------

func (s *Server) loginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Realm != "global" {
		req.Realm = "cn"
	}
	state, url, err := s.d.Client.DeviceAuthURL(req.Realm)
	if err != nil {
		writeErr(w, 502, "获取授权链接失败："+err.Error())
		return
	}
	s.mu.Lock()
	s.loginStates[state] = loginState{Realm: req.Realm, At: time.Now()}
	s.mu.Unlock()
	writeJSON(w, map[string]string{"state": state, "auth_url": url})
}

func (s *Server) loginPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.mu.Lock()
	ls, ok := s.loginStates[req.State]
	s.mu.Unlock()
	if !ok || time.Since(ls.At) > 10*time.Minute {
		writeErr(w, 410, "登录会话已过期，请重新开始")
		return
	}
	a, done, err := s.d.Client.DeviceAuthPoll(ls.Realm, req.State)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if !done {
		writeJSON(w, map[string]any{"done": false})
		return
	}
	c := s.d.Cfg.Snapshot()
	if err := auth.Save(s.d.AuthDir, a, c.Security.VaultEnabled, s.d.VaultPassword()); err != nil {
		writeErr(w, 500, "凭证保存失败："+err.Error())
		return
	}
	s.mu.Lock()
	delete(s.loginStates, req.State)
	s.mu.Unlock()
	s.d.ReloadAuths()
	// 新账号立即刷新余额
	if acc := s.d.Pool.Get(a.UID); acc != nil {
		go func() {
			remain, packs, err := s.d.Client.Balance(a)
			s.d.Pool.SetBalance(acc, remain, packs, err)
		}()
	}
	writeJSON(w, map[string]any{"done": true, "uid": a.UID, "nickname": a.Nickname, "realm": a.Realm})
}

// ---------- 密钥 ----------

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"keys": s.d.Keys.List()})
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string   `json:"name"`
		Models      []string `json:"models"`
		RPM         int      `json:"rpm"`
		DailyBudget float64  `json:"daily_budget"`
		TotalBudget float64  `json:"total_budget"`
		ExpireDays  int      `json:"expire_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Name == "" {
		req.Name = "未命名密钥"
	}
	var expires *time.Time
	if req.ExpireDays > 0 {
		t := time.Now().AddDate(0, 0, req.ExpireDays)
		expires = &t
	}
	k := s.d.Keys.Create(req.Name, req.Models, req.RPM, req.DailyBudget, req.TotalBudget, expires)
	writeJSON(w, k)
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name        *string   `json:"name"`
		Models      *[]string `json:"models"`
		RPM         *int      `json:"rpm"`
		DailyBudget *float64  `json:"daily_budget"`
		TotalBudget *float64  `json:"total_budget"`
		Disabled    *bool     `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ok := s.d.Keys.Update(id, func(k *keys.APIKey) {
		if req.Name != nil {
			k.Name = *req.Name
		}
		if req.Models != nil {
			k.Models = *req.Models
		}
		if req.RPM != nil {
			k.RPM = *req.RPM
		}
		if req.DailyBudget != nil {
			k.DailyBudget = *req.DailyBudget
		}
		if req.TotalBudget != nil {
			k.TotalBudget = *req.TotalBudget
		}
		if req.Disabled != nil {
			k.Disabled = *req.Disabled
		}
	})
	if !ok {
		writeErr(w, 404, "密钥不存在")
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	if !s.d.Keys.Delete(r.PathValue("id")) {
		writeErr(w, 404, "密钥不存在")
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	k, ok := s.d.Keys.Rotate(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "密钥不存在")
		return
	}
	writeJSON(w, k)
}

// ---------- 日志与用量 ----------

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	writeJSON(w, map[string]any{"logs": s.d.Stats.Query(stats.Filter{
		Model: q.Get("model"), UID: q.Get("uid"), KeyID: q.Get("key_id"),
		Status: q.Get("status"), Protocol: q.Get("protocol"), Limit: limit,
	})})
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"today": s.d.Stats.Today(), "last24h": s.d.Stats.Last24h(),
		"breakdown": s.d.Stats.Breakdown(), "heatmap": s.d.Stats.Heatmap(),
	})
}

// modelEntry 面板模型列表条目：上游目录原始值 + 本地覆盖 + 别名标记。
type modelEntry struct {
	upstream.Model
	Override config.ModelOverride `json:"override"`
	AliasOf  string               `json:"alias_of,omitempty"`
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	cfg := s.d.Cfg.Snapshot()
	var list []modelEntry
	for _, m := range s.d.Gateway.Catalog() {
		list = append(list, modelEntry{Model: m, Override: cfg.Gateway.ModelOverrides[m.ID]})
	}
	for alias, target := range cfg.Gateway.ModelAliases {
		list = append(list, modelEntry{Model: upstream.Model{ID: alias, Name: alias}, AliasOf: target})
	}
	writeJSON(w, map[string]any{"models": list})
}

// modelOverrideSave 保存/清除单模型覆盖（全零值 = 删除覆盖，回退上游默认）。
func (s *Server) modelOverrideSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req config.ModelOverride
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	req.Effort = strings.TrimSpace(req.Effort)
	err := s.d.Cfg.Update(func(c *config.Config) {
		if c.Gateway.ModelOverrides == nil {
			c.Gateway.ModelOverrides = map[string]config.ModelOverride{}
		}
		if req.ContextWindow <= 0 && req.MaxOutput <= 0 && (req.Effort == "" || req.Effort == "auto") {
			delete(c.Gateway.ModelOverrides, id)
		} else {
			c.Gateway.ModelOverrides[id] = req
		}
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// modelsRefresh 失效目录缓存并立即重新拉取。
func (s *Server) modelsRefresh(w http.ResponseWriter, r *http.Request) {
	s.d.Gateway.InvalidateCatalog()
	n := len(s.d.Gateway.Catalog())
	writeJSON(w, map[string]any{"count": n})
}

// ---------- 任务 ----------

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"tasks": s.d.Scheduler.List()})
}

func (s *Server) runTask(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Scheduler.RunNow(r.PathValue("name")); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "accepted"})
}

func (s *Server) taskHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"history": s.d.Scheduler.History()})
}

// ---------- 设置 ----------

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	c := s.d.Cfg.Snapshot()
	c.AdminKey = "***" // 不回显
	writeJSON(w, c)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req config.Config
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	err := s.d.Cfg.Update(func(c *config.Config) {
		c.Gateway = req.Gateway
		c.Pool = req.Pool
		c.Schedule = req.Schedule
		c.Notify = req.Notify
		c.Prompt = req.Prompt
		c.Security = req.Security
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) notifyTest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.d.Notifier.Test())
}

func (s *Server) notifyRecent(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"events": s.d.Notifier.Recent()})
}

// ---------- 诊断 ----------

func (s *Server) diagnose(w http.ResponseWriter, r *http.Request) {
	c := s.d.Cfg.Snapshot()
	type check struct {
		Name   string `json:"name"`
		Status string `json:"status"` // ok / warn / fail
		Detail string `json:"detail"`
	}
	var checks []check
	// 1. 配置
	checks = append(checks, check{"配置", "ok", fmt.Sprintf("监听 %s，数据目录 %s，凭证目录 %s", c.Listen, c.DataDir, s.d.AuthDir)})
	// 2. 目录可写
	testFile := filepath.Join(c.DataDir, ".write-test")
	if err := os.MkdirAll(c.DataDir, 0o755); err == nil && os.WriteFile(testFile, []byte("ok"), 0o600) == nil {
		_ = os.Remove(testFile)
		checks = append(checks, check{"数据目录可写", "ok", c.DataDir})
	} else {
		checks = append(checks, check{"数据目录可写", "fail", err.Error()})
	}
	// 3. 账号
	ready, total, balance, _ := s.d.Pool.Summary()
	st := "ok"
	if ready == 0 {
		st = "fail"
	}
	checks = append(checks, check{"账号池", st, fmt.Sprintf("%d/%d 可用，总余额 %d", ready, total, balance)})
	// 4. 凭证到期
	for _, a := range s.d.Pool.List() {
		status := "ok"
		if a.ExpiryDays < 3 {
			status = "fail"
		} else if a.ExpiryDays < 7 {
			status = "warn"
		}
		checks = append(checks, check{"凭证「" + a.Name + "」", status, fmt.Sprintf("剩余 %.1f 天（%s 到期）", a.ExpiryDays, a.TokenExpiresAt.Format("2006-01-02"))})
	}
	// 5. 网络与上游连通（用第一个账号试刷 token + 拉模型）
	accList := s.d.Pool.List()
	if len(accList) > 0 {
		acc := s.d.Pool.Get(accList[0].UID)
		start := time.Now()
		if err := s.d.Client.RefreshToken(acc.Auth); err != nil {
			checks = append(checks, check{"上游连通", "fail", "token 刷新失败："+err.Error()})
		} else {
			checks = append(checks, check{"上游连通", "ok", fmt.Sprintf("token 刷新成功，%dms", time.Since(start).Milliseconds())})
		}
		if models, err := s.d.Client.Models(acc.Auth); err != nil {
			checks = append(checks, check{"模型目录", "warn", err.Error()})
		} else {
			checks = append(checks, check{"模型目录", "ok", fmt.Sprintf("%d 个模型", len(models))})
		}
	}
	// 6. 时钟偏移（用上游 Date 头估算）
	if resp, err := s.d.Client.HTTP.Head("https://copilot.tencent.com"); err == nil {
		resp.Body.Close()
		if d := resp.Header.Get("Date"); d != "" {
			if t, err := http.ParseTime(d); err == nil {
				skew := time.Since(t)
				status := "ok"
				if skew > 5*time.Minute || skew < -5*time.Minute {
					status = "warn"
				}
				checks = append(checks, check{"时钟偏移", status, skew.Round(time.Second).String()})
			}
		}
	}
	// 7. 监听地址
	if host, port, err := net.SplitHostPort(c.Listen); err == nil {
		if host == "" || host == "0.0.0.0" || host == "::" {
			checks = append(checks, check{"监听地址", "warn", "监听全部网卡，确保 admin_key 与 API 密钥已设置"})
		} else {
			checks = append(checks, check{"监听地址", "ok", c.Listen + "（仅本机）"})
		}
		_ = port
	}
	writeJSON(w, map[string]any{"checks": checks})
}

// ---------- 备份与保险箱 ----------

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"backups": backup.ListBackups(filepath.Join(s.d.Cfg.Snapshot().DataDir, "backup"))})
}

func (s *Server) backupNow(w http.ResponseWriter, r *http.Request) {
	c := s.d.Cfg.Snapshot()
	target, err := backup.AutoBackup(s.d.AuthDir, s.d.StateFile, filepath.Join(c.DataDir, "backup"), c.Schedule.AutoBackupKeep)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "target": target})
}

func (s *Server) setVaultPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.d.SetVaultPass(req.Password)
	writeJSON(w, map[string]string{"status": "ok", "note": "主密码已生效（仅内存），新写入的凭证将用它加密"})
}

var _ = strings.TrimSpace
