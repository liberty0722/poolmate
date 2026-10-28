package panel

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"time"

	"poolmate/internal/auth"
	"poolmate/internal/localimport"
)

const switchWarning = "客户端与 PoolMate 现在共用同一份 refreshToken：任一方刷新令牌都会让另一方失效。切换后若客户端提示登录过期，请在客户端重新登录，或再次从这里切换。"

func (s *Server) switchBackupDir() string {
	return filepath.Join(s.d.Cfg.Snapshot().DataDir, "client-session-backup")
}

func findSource(name string) (localimport.Source, bool) {
	for _, src := range localimport.Discover() {
		if src.Name == name {
			return src, true
		}
	}
	return localimport.Source{}, false
}

// switchTargets 列出可切号的本机客户端、其当前账号与运行状态（不含令牌）。
func (s *Server) switchTargets(w http.ResponseWriter, r *http.Request) {
	type target struct {
		Name       string `json:"name"`
		Realm      string `json:"realm"`
		Running    bool   `json:"running"`
		UID        string `json:"uid"`
		Nickname   string `json:"nickname"`
		InPool     bool   `json:"in_pool"`
		HasBackup  bool   `json:"has_backup"`
		LoggedIn   bool   `json:"logged_in"`
		ReadErrMsg string `json:"error,omitempty"`
	}
	var out []target
	dir := s.switchBackupDir()
	for _, src := range localimport.Discover() {
		t := target{Name: src.Name, Realm: src.Realm, Running: localimport.ClientRunning(src), HasBackup: localimport.HasBackup(dir, src.Name)}
		if info, err := localimport.ReadSession(src); err != nil {
			t.ReadErrMsg = err.Error()
		} else if uid, nick, _ := info.Account(); uid != "" {
			t.LoggedIn, t.UID, t.Nickname = true, uid, nick
			t.InPool = s.d.Pool.Get(uid) != nil
		}
		out = append(out, t)
	}
	writeJSON(w, map[string]any{"targets": out, "warning": switchWarning})
}

// switchAccount 把账号池里的某个账号写入本机客户端。
func (s *Server) switchAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var req struct {
		Source      string `json:"source"`
		CloseClient bool   `json:"close_client"` // 客户端运行中时，是否允许由 PoolMate 关闭它
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	src, ok := findSource(req.Source)
	if !ok {
		writeErr(w, 404, "未找到本机客户端："+req.Source)
		return
	}
	acc := s.d.Pool.Get(uid)
	if acc == nil || acc.Auth == nil {
		writeErr(w, 404, "账号不存在")
		return
	}
	if r0 := auth.ResolveRealm(acc.Auth.Snapshot().Realm, acc.Auth.Snapshot().Domain); r0 != src.Realm {
		writeErr(w, 400, "账号区域（"+r0+"）与客户端「"+src.Name+"」（"+src.Realm+"）不一致，无法切换")
		return
	}

	// 先只读取当前会话作模板；读不到就中止，绝不在没有模板时硬写。
	cur, err := localimport.ReadSession(src)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if len(cur.Session) == 0 {
		writeErr(w, 400, "客户端「"+src.Name+"」当前未登录，缺少会话模板；请先在客户端登录任意账号一次")
		return
	}
	curUID, curNick, _ := cur.Account()
	if curUID == uid {
		writeJSON(w, map[string]any{"ok": true, "unchanged": true, "message": "客户端当前已经是这个账号"})
		return
	}
	if localimport.ClientRunning(src) && !req.CloseClient {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "客户端正在运行，需要先关闭才能安全切换", "need_close": true})
		return
	}

	// 令牌要新：先刷新并落盘，失败但仍有效则继续。
	c := s.d.Cfg.Snapshot()
	if err := s.d.Client.RefreshToken(acc.Auth); err != nil {
		if sn := acc.Auth.Snapshot(); sn.ExpiresAt-time.Now().Unix() < 120 {
			writeErr(w, 502, "刷新令牌失败且当前令牌即将过期："+err.Error())
			return
		}
	} else {
		_ = auth.Save(s.d.AuthDir, acc.Auth, c.Security.VaultEnabled, s.d.VaultPassword())
	}
	sn := acc.Auth.Snapshot()
	realm := auth.ResolveRealm(sn.Realm, sn.Domain)
	profile, err := s.d.Client.AccountRaw(realm, sn.AccessToken)
	if err != nil {
		writeErr(w, 502, "获取账号资料失败："+err.Error())
		return
	}

	// 通过所有前置校验后才关闭客户端。
	if localimport.ClientRunning(src) {
		if err := localimport.CloseClient(src, 10*time.Second); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		// 关闭后重新读取，拿到客户端退出前最后落盘的会话
		if cur, err = localimport.ReadSession(src); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}

	sess := localimport.BuildSession(cur.Session, localimport.Login{
		UID: sn.UID, Nickname: sn.Nickname, EnterpriseID: sn.EnterpriseID,
		AccessToken: sn.AccessToken, RefreshToken: sn.RefreshToken,
		Domain: sn.Domain, ExpiresAt: sn.ExpiresAt, Account: profile,
	})
	if _, err := localimport.WriteSession(src, cur, sess, s.switchBackupDir()); err != nil {
		writeErr(w, 500, "写入客户端失败："+err.Error())
		return
	}
	// 回读校验
	chk, err := localimport.ReadSession(src)
	if err != nil {
		writeErr(w, 500, "写入后回读失败："+err.Error())
		return
	}
	if gotUID, _, _ := chk.Account(); gotUID != uid {
		writeErr(w, 500, "写入后校验不一致，请使用「还原」恢复原账号")
		return
	}
	writeJSON(w, map[string]any{
		"ok": true, "source": src.Name, "from": curNick, "from_uid": curUID,
		"to": acc.Auth.DisplayName(), "has_backup": localimport.HasBackup(s.switchBackupDir(), src.Name),
		"message": "已切换，请重新打开客户端「" + src.Name + "」", "warning": switchWarning,
	})
}

// switchRestore 把客户端还原为上一次切换前的账号。
func (s *Server) switchRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source      string `json:"source"`
		CloseClient bool   `json:"close_client"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	src, ok := findSource(req.Source)
	if !ok {
		writeErr(w, 404, "未找到本机客户端："+req.Source)
		return
	}
	if !localimport.HasBackup(s.switchBackupDir(), src.Name) {
		writeErr(w, 404, "没有可还原的备份")
		return
	}
	if localimport.ClientRunning(src) {
		if !req.CloseClient {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "客户端正在运行，需要先关闭才能安全还原", "need_close": true})
			return
		}
		if err := localimport.CloseClient(src, 10*time.Second); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	uid, nick, err := localimport.RestoreSession(src, s.switchBackupDir())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "uid": uid, "nickname": nick, "message": "已还原，请重新打开客户端「" + src.Name + "」"})
}
