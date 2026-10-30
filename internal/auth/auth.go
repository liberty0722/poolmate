// Package auth 管理 WorkBuddy 账号凭证：与 workbuddy2api-panel / PoolService
// 的 auths/workbuddy-<uid>.json 文件格式完全兼容，可选 vault 加密落盘（.enc）。
package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"poolmate/internal/vault"
)

// Auth 一个账号的凭证。锁保护 Token 字段的并发读写（刷新与出站并发）。
type Auth struct {
	mu sync.Mutex
	// refreshMu 串行化同一账号的 token 刷新：上游每次刷新都会轮换 refresh token，
	// 并发刷新会让后到的请求拿着已作废的旧 refresh token 而失败。
	refreshMu   sync.Mutex
	lastRefresh time.Time // 最近一次成功刷新（仅内存）
	refreshErr  string    // 最近一次刷新失败原因，成功后清空（仅内存）

	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterpriseId"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // access token 过期 Unix 秒
	Domain       string `json:"domain"`
	Realm        string `json:"realm"` // cn / global
}

// fileV1 与 workbuddy2api 系凭证文件完全同构。
type fileV1 struct {
	Account struct {
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
		UID          string `json:"uid"`
	} `json:"account"`
	Auth struct {
		AccessToken  string `json:"accessToken"`
		Domain       string `json:"domain"`
		ExpiresAt    int64  `json:"expiresAt"`
		Realm        string `json:"realm"`
		RefreshToken string `json:"refreshToken"`
	} `json:"auth"`
}

// ResolveRealm 按显式值或域名推断 realm。
func ResolveRealm(realm, domain string) string {
	r := strings.ToLower(strings.TrimSpace(realm))
	if r == "global" || r == "cn" {
		return r
	}
	if strings.Contains(domain, "workbuddy.ai") {
		return "global"
	}
	return "cn"
}

// Parse 从 JSON 字节解析凭证（自动识别明文 / vault 密文；密文需密码）。
func Parse(raw []byte, password string) (*Auth, error) {
	// 兼容带 UTF-8 BOM 的文件（部分工具导出时会加）。
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if vault.IsEncrypted(raw) {
		plain, err := vault.Decrypt(raw, password)
		if err != nil && password != "" {
			// 设主密码之前写入的文件只用了机器因子：回退无密码再试一次，避免设密码后账号读不出来
			if p2, err2 := vault.Decrypt(raw, ""); err2 == nil {
				plain, err = p2, nil
			}
		}
		if err != nil {
			return nil, err
		}
		raw = plain
	}
	var f fileV1
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse auth: %w", err)
	}
	if f.Account.UID == "" || f.Auth.RefreshToken == "" {
		return nil, errors.New("凭证缺少 uid 或 refreshToken")
	}
	return &Auth{
		UID:          f.Account.UID,
		Nickname:     f.Account.Nickname,
		EnterpriseID: f.Account.EnterpriseID,
		AccessToken:  f.Auth.AccessToken,
		RefreshToken: f.Auth.RefreshToken,
		ExpiresAt:    f.Auth.ExpiresAt,
		Domain:       f.Auth.Domain,
		Realm:        ResolveRealm(f.Auth.Realm, f.Auth.Domain),
	}, nil
}

// Marshal 序列化为兼容格式；encrypt 非空时用该密码加密。
func (a *Auth) Marshal(encrypt bool, password string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var f fileV1
	f.Account.UID = a.UID
	f.Account.Nickname = a.Nickname
	f.Account.EnterpriseID = a.EnterpriseID
	f.Auth.AccessToken = a.AccessToken
	f.Auth.RefreshToken = a.RefreshToken
	f.Auth.ExpiresAt = a.ExpiresAt
	f.Auth.Domain = a.Domain
	f.Auth.Realm = a.Realm
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	if encrypt {
		return vault.Encrypt(raw, password)
	}
	return raw, nil
}

// LoadDir 读取目录下全部凭证（.json 明文 / .json.enc 密文），返回按 uid 索引的表。
// 单个文件损坏不致命：记录进 errs 继续。
func LoadDir(dir, password string) (map[string]*Auth, []error) {
	out := map[string]*Auth{}
	var errs []error
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, []error{err}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".json.enc") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		a, err := Parse(raw, password)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		// 同 uid 保留 access token 较新的一方。
		if old, ok := out[a.UID]; !ok || a.ExpiresAt > old.ExpiresAt {
			out[a.UID] = a
		}
	}
	return out, errs
}

// safeUID 拒绝可能逃出凭证目录的 UID（来自上游/导入文件，不可信）。
func safeUID(uid string) error {
	if uid == "" || uid == "." || strings.Contains(uid, "..") || strings.ContainsAny(uid, "/\\:*?\"<>|") {
		return fmt.Errorf("非法账号 UID %q", uid)
	}
	for _, r := range uid {
		if r < 0x20 {
			return fmt.Errorf("非法账号 UID %q", uid)
		}
	}
	return nil
}

// Save 原子落盘（0600，临时文件 → fsync → 替换）；encrypt 时写 .json.enc 并删除旧明文，反之亦然。
func Save(dir string, a *Auth, encrypt bool, password string) error {
	if err := safeUID(a.UID); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := a.Marshal(encrypt, password)
	if err != nil {
		return err
	}
	base := filepath.Join(dir, fmt.Sprintf("workbuddy-%s.json", a.UID))
	path, other := base, base+".enc"
	if encrypt {
		path, other = base+".enc", base
	}
	f, err := os.CreateTemp(dir, ".workbuddy-*.tmp") // 每次独立临时文件，并发保存不互相踩
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // 成功 Rename 后此处为空操作
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// 清掉另一种形态的旧文件，避免双份（尤其是开启加密后残留的明文）。
	_ = os.Remove(other)
	return nil
}

// NeedsRewrite 凭证目录里是否存在与当前加密设置不一致的文件：
// 开启加密时有明文或旧格式密文；关闭加密时有密文。
func NeedsRewrite(dir string, encrypt bool) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "workbuddy-") {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".json.enc"):
			if !encrypt {
				return true
			}
			if raw, err := os.ReadFile(filepath.Join(dir, name)); err == nil && vault.IsLegacy(raw) {
				return true
			}
		case strings.HasSuffix(name, ".json"):
			if encrypt {
				return true
			}
		}
	}
	return false
}

// Rewrite 按当前加密设置重写一个账号的凭证文件（持刷新锁，不与 token 刷新落盘交错）。
func Rewrite(dir string, a *Auth, encrypt bool, password string) error {
	a.LockRefresh()
	defer a.UnlockRefresh()
	return Save(dir, a, encrypt, password)
}

// Delete 删除某账号的凭证文件（两种形态）。
func Delete(dir, uid string) error {
	if err := safeUID(uid); err != nil {
		return err
	}
	base := filepath.Join(dir, fmt.Sprintf("workbuddy-%s.json", uid))
	err1 := os.Remove(base)
	err2 := os.Remove(base + ".enc")
	if err1 != nil && !os.IsNotExist(err1) {
		return err1
	}
	if err2 != nil && !os.IsNotExist(err2) {
		return err2
	}
	return nil
}

// ListFiles 返回目录下凭证文件名（排序，供导出枚举）。
func ListFiles(dir string) []string {
	var names []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".json.enc") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Snapshot 并发安全的字段拷贝。
type Snapshot struct {
	UID, Nickname, EnterpriseID string
	AccessToken, RefreshToken   string
	ExpiresAt                   int64
	Domain, Realm               string
}

func (a *Auth) Snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Snapshot{
		UID: a.UID, Nickname: a.Nickname, EnterpriseID: a.EnterpriseID,
		AccessToken: a.AccessToken, RefreshToken: a.RefreshToken,
		ExpiresAt: a.ExpiresAt, Domain: a.Domain, Realm: a.Realm,
	}
}

// ApplyTokens 刷新成功后写回（compare-and-swap：锁外另一刷新已完成则放弃）。
func (a *Auth) ApplyTokens(atBefore, rtBefore, at, rt, domain string, expiresIn int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// access token 每次刷新必变；refresh token 不一定轮换，不能作为判断依据。
	if a.AccessToken != atBefore {
		return
	}
	a.AccessToken = at
	if rt != "" {
		a.RefreshToken = rt
	}
	if domain != "" {
		a.Domain = domain
	}
	if expiresIn > 0 && expiresIn < 10*365*24*3600 {
		a.ExpiresAt = time.Now().Unix() + expiresIn
	}
}

// LockRefresh / UnlockRefresh 串行化同一账号的 token 刷新。
func (a *Auth) LockRefresh()   { a.refreshMu.Lock() }
func (a *Auth) UnlockRefresh() { a.refreshMu.Unlock() }

// SetRefreshResult 记录一次刷新结果：成功更新时间并清空错误，失败记录原因。
func (a *Auth) SetRefreshResult(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.refreshErr = err.Error()
		return
	}
	a.lastRefresh, a.refreshErr = time.Now(), ""
}

// RefreshStatus 返回最近一次成功刷新时间与最近一次失败原因（空 = 最近一次成功或未尝试）。
func (a *Auth) RefreshStatus() (time.Time, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastRefresh, a.refreshErr
}

// NeedsRefresh 报告 access token 是否在 skew 内到期。
func (a *Auth) NeedsRefresh(skew time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Add(skew).Unix() >= a.ExpiresAt
}

// Expiry 返回 access token 过期时刻与剩余天数。
func (a *Auth) Expiry() (time.Time, float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := time.Unix(a.ExpiresAt, 0)
	return t, time.Until(t).Hours() / 24
}

// DisplayName 面板展示名。
func (a *Auth) DisplayName() string {
	s := a.Snapshot()
	if s.Nickname != "" {
		return s.Nickname
	}
	if len(s.UID) > 8 {
		return s.UID[:8]
	}
	return s.UID
}
