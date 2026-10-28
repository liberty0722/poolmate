package panel

import (
	"encoding/json"
	"net/http"
	"time"

	"poolmate/internal/auth"
	"poolmate/internal/localimport"
	"poolmate/internal/pool"
)

// refreshMeta 采集付费类型与用量提醒（失败静默，不影响主流程）。
func (s *Server) refreshMeta(acc *pool.Account) {
	pay, perr := s.d.Client.PaymentType(acc.Auth)
	dn, derr := s.d.Client.DosageNotify(acc.Auth)
	if perr != nil && derr != nil {
		return
	}
	code, msg := 0, ""
	if derr == nil && dn != nil {
		code, msg = dn.Code, dn.Zh
		if msg == "" {
			msg = dn.En
		}
	}
	if perr != nil {
		pay = ""
	}
	s.d.Pool.SetMeta(acc, pay, code, msg)
}

// localSources 列出本机可导入的客户端及其中的账号（仅摘要，不含令牌）。
func (s *Server) localSources(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Source       string `json:"source"`
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		EnterpriseID string `json:"enterprise_id"`
		Realm        string `json:"realm"`
		Domain       string `json:"domain"`
		ExpiresAt    int64  `json:"expires_at"`
		InPool       bool   `json:"in_pool"`
	}
	var items []item
	var errs []string
	srcs := localimport.Discover()
	for _, src := range srcs {
		toks, err := localimport.Extract(src)
		if err != nil {
			errs = append(errs, src.Name+"："+err.Error())
			continue
		}
		for _, t := range toks {
			items = append(items, item{
				Source: src.Name, UID: t.UID, Nickname: t.Nickname, EnterpriseID: t.EnterpriseID,
				Realm: t.Realm, Domain: t.Domain, ExpiresAt: t.ExpiresAt,
				InPool: t.UID != "" && s.d.Pool.Get(t.UID) != nil,
			})
		}
	}
	writeJSON(w, map[string]any{"sources": srcs, "accounts": items, "errors": errs})
}

// localImport 把本机客户端中的登录态导入账号池。
// 同一 UID 在多个客户端出现时，保留过期时间最晚的一份；池中已有更新凭证则跳过。
func (s *Server) localImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sources []string `json:"sources"` // 为空 = 全部
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	want := map[string]bool{}
	for _, n := range req.Sources {
		want[n] = true
	}
	best := map[string]localimport.Token{}
	var skipped []string
	for _, src := range localimport.Discover() {
		if len(want) > 0 && !want[src.Name] {
			continue
		}
		toks, err := localimport.Extract(src)
		if err != nil {
			skipped = append(skipped, src.Name+"："+err.Error())
			continue
		}
		for _, t := range toks {
			// 补全账号信息：本地缺 uid 时凭 token 向上游查询
			if t.UID == "" {
				p, perr := s.d.Client.ProfileByToken(t.Realm, t.AccessToken)
				if perr != nil {
					skipped = append(skipped, src.Name+"：无法确定账号 uid（"+perr.Error()+"）")
					continue
				}
				t.UID, t.Nickname, t.EnterpriseID = p.UID, p.Nickname, p.EnterpriseID
			}
			if old, ok := best[t.UID]; !ok || t.ExpiresAt > old.ExpiresAt {
				best[t.UID] = t
			}
		}
	}
	c := s.d.Cfg.Snapshot()
	var imported []string
	for uid, t := range best {
		exp := t.ExpiresAt
		if exp == 0 {
			exp = time.Now().Add(24 * time.Hour).Unix()
		}
		a := &auth.Auth{
			UID: uid, Nickname: t.Nickname, EnterpriseID: t.EnterpriseID,
			AccessToken: t.AccessToken, RefreshToken: t.RefreshToken,
			ExpiresAt: exp, Domain: t.Domain,
			Realm: auth.ResolveRealm(t.Realm, t.Domain),
		}
		if old := s.d.Pool.Get(uid); old != nil && old.Auth != nil && old.Auth.Snapshot().ExpiresAt >= exp {
			skipped = append(skipped, a.DisplayName()+"（池中凭证更新，跳过）")
			continue
		}
		if err := auth.Save(s.d.AuthDir, a, c.Security.VaultEnabled, s.d.VaultPassword()); err != nil {
			skipped = append(skipped, a.DisplayName()+" 保存失败："+err.Error())
			continue
		}
		imported = append(imported, a.DisplayName())
	}
	s.d.ReloadAuths()
	for uid := range best {
		if acc := s.d.Pool.Get(uid); acc != nil && acc.Auth != nil {
			go func() {
				remain, packs, err := s.d.Client.Balance(acc.Auth)
				s.d.Pool.SetBalance(acc, remain, packs, err)
				if err == nil {
					s.refreshMeta(acc)
				}
			}()
		}
	}
	writeJSON(w, map[string]any{
		"imported": imported, "skipped": skipped,
		"warning": "本机客户端与 PoolMate 共用同一份 refreshToken；任一方刷新令牌都可能使另一方登录失效，如客户端被登出请重新登录。",
	})
}
