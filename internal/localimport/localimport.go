// Package localimport 从本机已登录的 WorkBuddy / CodeBuddy 客户端导入账号凭证。
// 客户端（VS Code 系 Electron 应用）把令牌放在 globalStorage/state.vscdb 的
// secret:// 条目里，值经 Electron safeStorage 加密（Windows：DPAPI 保护的 AES-256-GCM 主密钥）。
// 本包只读，不会修改客户端数据，也不会在日志中输出明文令牌。
package localimport

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Source 一个可导入的本机客户端数据目录。
type Source struct {
	Name  string `json:"name"` // 例如 CodeBuddy CN
	Dir   string `json:"dir"`
	Realm string `json:"realm"` // cn / global（按客户端名推断，最终以 token 的 domain 为准）
}

// Token 从客户端中解出的一组凭证（未补全账号信息）。
type Token struct {
	Source       string `json:"source"`
	Key          string `json:"key"`
	AccessToken  string `json:"-"`
	RefreshToken string `json:"-"`
	ExpiresAt    int64  `json:"expires_at"` // Unix 秒，0 表示未知
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterprise_id"`
}

const extensionID = "tencent-cloud.coding-copilot"

// Discover 探测本机已安装的客户端数据目录。
func Discover() []Source {
	if runtime.GOOS != "windows" {
		return nil
	}
	root := os.Getenv("APPDATA")
	if root == "" {
		return nil
	}
	cands := []struct{ name, realm string }{
		{"CodeBuddy CN", "cn"},
		{"CodeBuddy CN Insiders", "cn"},
		{"WorkBuddy", "cn"},
		{"CodeBuddy", "global"},
		{"CodeBuddy Insiders", "global"},
		{"WorkBuddy AI", "global"},
	}
	var out []Source
	for _, c := range cands {
		d := filepath.Join(root, c.name)
		if _, err := os.Stat(filepath.Join(d, "User", "globalStorage", "state.vscdb")); err == nil {
			out = append(out, Source{Name: c.name, Dir: d, Realm: c.realm})
		}
	}
	return out
}

// masterKey 从 Local State 取并解开 safeStorage 主密钥。
func masterKey(dir string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "Local State"))
	if err != nil {
		return nil, fmt.Errorf("读取 Local State: %w", err)
	}
	var ls struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil || ls.OSCrypt.EncryptedKey == "" {
		return nil, errors.New("Local State 中无 os_crypt.encrypted_key")
	}
	blob, err := base64.StdEncoding.DecodeString(ls.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("encrypted_key base64: %w", err)
	}
	if len(blob) < 5 || string(blob[:5]) != "DPAPI" {
		return nil, errors.New("encrypted_key 不是 DPAPI 格式")
	}
	key, err := dpapiUnprotect(blob[5:])
	if err != nil {
		return nil, fmt.Errorf("DPAPI 解密主密钥失败（需用同一 Windows 用户运行）: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("主密钥长度异常: %d", len(key))
	}
	return key, nil
}

// decryptV10 解 Chromium/Electron 的 v10 AES-256-GCM 值。
func decryptV10(key, enc []byte) ([]byte, error) {
	if len(enc) < 3+12+16 || string(enc[:3]) != "v10" {
		return nil, errors.New("非 v10 密文")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	nonce, ct := enc[3:15], enc[15:]
	return gcm.Open(nil, nonce, ct, nil)
}

// bufferBytes 把 VS Code 存储的 {"type":"Buffer","data":[...]} 还原为字节；
// 若本身就是原始字节则原样返回。
func bufferBytes(v []byte) []byte {
	s := strings.TrimSpace(string(v))
	if strings.HasPrefix(s, "{") {
		var b struct {
			Type string `json:"type"`
			Data []int  `json:"data"`
		}
		if json.Unmarshal([]byte(s), &b) == nil && b.Type == "Buffer" {
			out := make([]byte, len(b.Data))
			for i, x := range b.Data {
				out[i] = byte(x)
			}
			return out
		}
	}
	return v
}

// secrets 读取并解密一个客户端目录下属于本扩展的全部 secret（键 → 明文）。
func secrets(src Source) (map[string][]byte, error) {
	key, err := masterKey(src.Dir)
	if err != nil {
		return nil, err
	}
	db, err := openSQLite(filepath.Join(src.Dir, "User", "globalStorage", "state.vscdb"))
	if err != nil {
		return nil, fmt.Errorf("读取 state.vscdb: %w", err)
	}
	items, err := db.itemTable(func(k string) bool {
		return strings.HasPrefix(k, "secret://") && strings.Contains(k, extensionID)
	})
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for k, v := range items {
		name := k
		var meta struct {
			Key string `json:"key"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(k, "secret://")), &meta) == nil && meta.Key != "" {
			name = meta.Key
		}
		if !strings.Contains(strings.ToLower(name), "accesstoken") {
			continue
		}
		plain, err := decryptV10(key, bufferBytes(v))
		if err != nil {
			continue
		}
		out[name] = plain
	}
	if len(out) == 0 {
		return nil, errors.New("未找到可解密的登录凭证（客户端可能未登录）")
	}
	return out, nil
}

// Extract 解出该客户端中所有账号凭证。
func Extract(src Source) ([]Token, error) {
	sec, err := secrets(src)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(sec))
	for k := range sec {
		names = append(names, k)
	}
	sort.Strings(names)
	var toks []Token
	for _, name := range names {
		if !strings.Contains(strings.ToLower(name), "accesstoken") {
			continue
		}
		for _, t := range parseSecret(sec[name]) {
			t.Source, t.Key = src.Name, name
			if t.Realm == "" {
				t.Realm = src.Realm
				if strings.HasSuffix(name, "cn") {
					t.Realm = "cn"
				}
			}
			toks = append(toks, t)
		}
	}
	if len(toks) == 0 {
		return nil, errors.New("凭证条目存在但未解析出 accessToken")
	}
	return toks, nil
}

// Describe 调试用：列出解密后各 secret 的键与 JSON 结构（只给键名与长度，不含明文）。
func Describe(src Source) (map[string]string, error) {
	sec, err := secrets(src)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range sec {
		var x any
		if json.Unmarshal(v, &x) == nil {
			out[k] = fmt.Sprintf("json len=%d shape=%s", len(v), shape(x, 0))
		} else {
			out[k] = fmt.Sprintf("text len=%d", len(v))
		}
	}
	return out, nil
}

func shape(v any, d int) string {
	if d > 3 {
		return "…"
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+":"+shape(t[k], d+1))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		if len(t) == 0 {
			return "[]"
		}
		return fmt.Sprintf("[%d x %s]", len(t), shape(t[0], d+1))
	case string:
		return "str"
	case float64:
		return "num"
	case bool:
		return "bool"
	}
	return "null"
}

// parseSecret 宽松解析：明文可能是纯 token、单个对象或对象数组（多账号）。
func parseSecret(plain []byte) []Token {
	s := strings.TrimSpace(string(plain))
	if s == "" {
		return nil
	}
	var x any
	if err := json.Unmarshal([]byte(s), &x); err != nil {
		if strings.Count(s, ".") == 2 && !strings.ContainsAny(s, " \n\"{") {
			return []Token{{AccessToken: s}}
		}
		return nil
	}
	if str, ok := x.(string); ok {
		// 二次编码的 JSON
		var y any
		if json.Unmarshal([]byte(str), &y) == nil {
			x = y
		} else if strings.Count(str, ".") == 2 {
			return []Token{{AccessToken: str}}
		}
	}
	var out []Token
	collect(x, &out, 0)
	return out
}

func collect(v any, out *[]Token, depth int) {
	if depth > 4 {
		return
	}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			collect(e, out, depth+1)
		}
	case map[string]any:
		if at := pickStr(t, "accessToken", "access_token"); at != "" {
			tk := Token{
				AccessToken:  at,
				RefreshToken: pickStr(t, "refreshToken", "refresh_token"),
				Domain:       pickStr(t, "domain"),
				UID:          pickStr(t, "uid"),
				Nickname:     pickStr(t, "nickname"),
				EnterpriseID: pickStr(t, "enterpriseId", "enterprise_id"),
				ExpiresAt:    pickTime(t),
			}
			if au, ok := t["auth"].(map[string]any); ok {
				if tk.RefreshToken == "" {
					tk.RefreshToken = pickStr(au, "refreshToken")
				}
				if tk.Domain == "" {
					tk.Domain = pickStr(au, "domain")
				}
				if tk.ExpiresAt == 0 {
					tk.ExpiresAt = pickTime(au)
				}
			}
			if acc, ok := t["account"].(map[string]any); ok {
				if v := pickStr(acc, "uid"); v != "" {
					tk.UID = v
				}
				if v := pickStr(acc, "nickname", "displayName"); v != "" {
					tk.Nickname = v
				}
				if v := pickStr(acc, "enterpriseId"); v != "" {
					tk.EnterpriseID = v
				}
			}
			*out = append(*out, tk)
			return
		}
		for _, e := range t {
			collect(e, out, depth+1)
		}
	}
}

func pickStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func pickTime(m map[string]any) int64 {
	for _, k := range []string{"expiresAt", "expires_at", "expiredAt", "expireTime"} {
		if f, ok := m[k].(float64); ok && f > 0 {
			if f > 1e12 {
				return int64(f) / 1000
			}
			return int64(f)
		}
	}
	return 0
}
