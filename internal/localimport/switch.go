package localimport

// 本文件实现“一键切号”：把账号池里某个账号的登录态写进本机 WorkBuddy / CodeBuddy 客户端。
//
// 客户端的登录会话是 state.vscdb 中一条 secret 记录（键名 planning-genie.new.accessToken[cn]），
// 值为 Electron safeStorage 加密（v10 + AES-256-GCM）后再以 {"type":"Buffer","data":[...]} 文本存放。
// 写入流程与读取完全对称：用同一把 DPAPI 主密钥加密新会话，再 INSERT OR REPLACE 回 ItemTable。
//
// 安全约束：
//   - 调用方必须先确认客户端已退出，否则客户端内存里的旧会话会在下次刷新时覆盖本次写入；
//   - 写入前一定把原记录（仍是密文）备份到 backupDir，可通过 RestoreSession 原样还原；
//   - 全程不输出任何令牌明文。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，仅用于写回（读取仍走无依赖的只读解析器）
)

// sessionKeyBase 会话 secret 的键名前缀；国内版带 cn 后缀，国际版不带。
const sessionKeyBase = "planning-genie.new.accessToken"

// maxBackups 每个客户端最多保留的会话备份份数。
const maxBackups = 10

// Login 要写入客户端的登录信息（来自账号池）。
type Login struct {
	UID, Nickname, EnterpriseID string
	AccessToken, RefreshToken   string
	Domain                      string
	ExpiresAt                   int64          // access token 过期 Unix 秒
	Account                     map[string]any // 上游 /console/login/account 的完整资料；为空则用最小资料兜底
}

// SessionInfo 客户端当前会话的读取结果。
type SessionInfo struct {
	RawKey   string         // ItemTable 中的原始键
	RawValue string         // ItemTable 中的原始值（密文，可原样备份与还原）
	Session  map[string]any // 解密后的会话；客户端未登录时为空
}

// Account 返回会话里当前账号的摘要。
func (si *SessionInfo) Account() (uid, nickname, enterpriseID string) {
	ac, _ := si.Session["account"].(map[string]any)
	return strOf(ac, "uid"), strOf(ac, "nickname"), strOf(ac, "enterpriseId")
}

func statePath(src Source) string {
	return filepath.Join(src.Dir, "User", "globalStorage", "state.vscdb")
}

func rawSecretKey(name string) string {
	return `secret://{"extensionId":"` + extensionID + `","key":"` + name + `"}`
}

func secretName(rawKey string) string {
	var meta struct {
		Key string `json:"key"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(rawKey, "secret://")), &meta) == nil {
		return meta.Key
	}
	return ""
}

// pickSessionKey 在已有的 secret 键里挑出会话键；没有则按版本推断默认键名。
func pickSessionKey(src Source, items map[string][]byte) string {
	wantCN := src.Realm == "cn"
	var any string
	for rk := range items {
		name := secretName(rk)
		if !strings.HasPrefix(name, sessionKeyBase) {
			continue
		}
		if strings.HasSuffix(name, "cn") == wantCN {
			return rk
		}
		any = rk
	}
	if any != "" {
		return any
	}
	if wantCN {
		return rawSecretKey(sessionKeyBase + "cn")
	}
	return rawSecretKey(sessionKeyBase)
}

// ReadSession 读取并解密客户端当前会话（只读，客户端运行中也可调用）。
func ReadSession(src Source) (*SessionInfo, error) {
	db, err := openSQLite(statePath(src))
	if err != nil {
		return nil, fmt.Errorf("读取 state.vscdb: %w", err)
	}
	items, err := db.itemTable(func(k string) bool {
		return strings.HasPrefix(k, "secret://") && strings.Contains(k, extensionID) && strings.Contains(k, sessionKeyBase)
	})
	if err != nil {
		return nil, err
	}
	info := &SessionInfo{RawKey: pickSessionKey(src, items), Session: map[string]any{}}
	v, ok := items[info.RawKey]
	if !ok {
		return info, nil
	}
	key, err := masterKey(src.Dir)
	if err != nil {
		return nil, err
	}
	plain, err := decryptV10(key, bufferBytes(v))
	if err != nil {
		return nil, fmt.Errorf("解密当前会话失败: %w", err)
	}
	info.RawValue = string(v)
	if err := json.Unmarshal(plain, &info.Session); err != nil {
		return nil, fmt.Errorf("当前会话不是合法 JSON: %w", err)
	}
	return info, nil
}

// BuildSession 以客户端现有会话 tpl 为模板，换入 l 这个账号，得到新会话。
// 令牌、过期时间、sessionState 等全部由令牌自身的 JWT 声明推出，不靠猜测；
// account 优先用上游返回的完整资料。
func BuildSession(tpl map[string]any, l Login) map[string]any {
	sess := cloneMap(tpl)
	nowMs := time.Now().UnixMilli()

	ac, rc := jwtClaims(l.AccessToken), jwtClaims(l.RefreshToken)
	expMs := l.ExpiresAt * 1000
	if exp := numClaim(ac, "exp"); exp > 0 {
		expMs = exp * 1000
	}
	iat := numClaim(ac, "iat")

	au, _ := sess["auth"].(map[string]any)
	if au == nil {
		au = map[string]any{}
	}
	au["accessToken"], au["refreshToken"], au["domain"] = l.AccessToken, l.RefreshToken, l.Domain
	au["expiresAt"] = expMs
	au["tokenType"] = "Bearer"
	if iat > 0 {
		au["lastRefreshTime"] = iat * 1000
		if expMs/1000 > iat {
			au["expiresIn"] = expMs/1000 - iat
		}
	} else {
		au["lastRefreshTime"] = nowMs
	}
	delete(au, "refreshExpiresAt")
	delete(au, "refreshExpiresIn")
	if rexp := numClaim(rc, "exp"); rexp > 0 {
		au["refreshExpiresAt"] = rexp * 1000
		if riat := numClaim(rc, "iat"); riat > 0 && rexp > riat {
			au["refreshExpiresIn"] = rexp - riat
		}
	}
	if sid := strOf(ac, "sid"); sid != "" {
		au["sessionState"] = sid
	} else {
		delete(au, "sessionState")
	}
	sess["auth"] = au

	// 个人账号的会话里顶层令牌与 auth 同值；converted 仅用于企业 SSO 令牌转换，这里置 false。
	sess["accessToken"], sess["token"], sess["refreshToken"] = l.AccessToken, l.AccessToken, l.RefreshToken
	sess["domain"], sess["expiresAt"], sess["converted"] = l.Domain, expMs, false

	acct := cloneMap(l.Account)
	if len(acct) == 0 {
		acct = map[string]any{
			"uid": l.UID, "nickname": l.Nickname, "displayName": l.Nickname, "enterpriseId": l.EnterpriseID,
			"pluginEnabled": true, "isFirstLogin": false, "logOutEnable": true,
		}
	}
	acct["uid"] = l.UID
	if strOf(acct, "id") == "" {
		acct["id"] = l.UID
	}
	acct["lastLogin"] = true
	sess["account"] = acct
	sess["accounts"] = []any{cloneMap(acct)}
	return sess
}

// WriteSession 把新会话加密写回客户端存储。调用方须保证客户端已退出。
// 写入前把原记录备份到 backupDir，返回备份文件路径（原先未登录时为空）。
func WriteSession(src Source, info *SessionInfo, sess map[string]any, backupDir string) (string, error) {
	key, err := masterKey(src.Dir)
	if err != nil {
		return "", err
	}
	val, err := sealSession(key, sess)
	if err != nil {
		return "", err
	}
	bak := ""
	if info.RawValue != "" {
		if bak, err = saveBackup(backupDir, src.Name, info.RawKey, info.RawValue); err != nil {
			return "", fmt.Errorf("备份原会话失败（已中止，未改动客户端）: %w", err)
		}
	}
	if err := putRaw(statePath(src), info.RawKey, val); err != nil {
		return bak, err
	}
	return bak, nil
}

// RestoreSession 把最近一次备份的原会话还原到客户端。
// 还原前会把当前会话再备份一份，所以再次还原等于撤销这次还原。返回被还原账号的摘要。
func RestoreSession(src Source, backupDir string) (uid, nickname string, err error) {
	path, err := latestBackup(backupDir, src.Name)
	if err != nil {
		return "", "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var bf backupFile
	if err := json.Unmarshal(raw, &bf); err != nil || bf.Key == "" || bf.Value == "" {
		return "", "", errors.New("备份文件损坏")
	}
	if cur, cerr := ReadSession(src); cerr == nil && cur.RawValue != "" {
		if _, err := saveBackup(backupDir, src.Name, cur.RawKey, cur.RawValue); err != nil {
			return "", "", fmt.Errorf("备份当前会话失败（已中止）: %w", err)
		}
	}
	if err := putRaw(statePath(src), bf.Key, bf.Value); err != nil {
		return "", "", err
	}
	_ = os.Remove(path)
	if now, rerr := ReadSession(src); rerr == nil {
		uid, nickname, _ = now.Account()
	}
	return uid, nickname, nil
}

// HasBackup 是否存在可还原的备份。
func HasBackup(backupDir, srcName string) bool {
	_, err := latestBackup(backupDir, srcName)
	return err == nil
}

// ---------- 加密 / 存储 ----------

// sealSession 序列化并按 Electron safeStorage 的 v10 格式加密，返回 Buffer JSON 文本。
func sealSession(masterKey []byte, sess map[string]any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(sess); err != nil {
		return "", err
	}
	plain := bytes.TrimRight(buf.Bytes(), "\n")
	blk, err := aes.NewCipher(masterKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	blob := append([]byte("v10"), nonce...)
	blob = append(blob, gcm.Seal(nil, nonce, plain, nil)...)
	return bufferJSON(blob), nil
}

// bufferJSON 等价于 JS 的 JSON.stringify(Buffer)。
func bufferJSON(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b)*4 + 32)
	sb.WriteString(`{"type":"Buffer","data":[`)
	for i, x := range b {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.Itoa(int(x)))
	}
	sb.WriteString("]}")
	return sb.String()
}

// putRaw 以 TEXT 写入 ItemTable（VS Code 的 secret 就是以文本形式存放）。
func putRaw(dbPath, rawKey, value string) error {
	if _, err := os.Stat(dbPath); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout = 8000"); err != nil {
		return err
	}
	if _, err := db.Exec("INSERT OR REPLACE INTO ItemTable (key, value) VALUES (?, ?)", rawKey, value); err != nil {
		return fmt.Errorf("写入 state.vscdb 失败（客户端是否仍在运行？）: %w", err)
	}
	// 合并 WAL，让其它进程（含我们自己的只读解析器）立即看到一致的主库。
	_, _ = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return nil
}

// ---------- 备份 ----------

type backupFile struct {
	Source string `json:"source"`
	Key    string `json:"key"`
	Value  string `json:"value"` // 原始密文文本，与 ItemTable 中完全一致
	Time   string `json:"time"`
}

func backupPrefix(srcName string) string {
	r := strings.NewReplacer(" ", "_", "/", "_", "\\", "_", ":", "_")
	return r.Replace(srcName) + "-"
}

func saveBackup(dir, srcName, rawKey, value string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	now := time.Now()
	path := filepath.Join(dir, backupPrefix(srcName)+now.Format("20060102-150405.000")+".json")
	data, err := json.Marshal(backupFile{Source: srcName, Key: rawKey, Value: value, Time: now.Format(time.RFC3339)})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	names := listBackups(dir, srcName)
	for len(names) > maxBackups {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
	return path, nil
}

// listBackups 升序（旧→新）。
func listBackups(dir, srcName string) []string {
	entries, _ := os.ReadDir(dir)
	prefix := backupPrefix(srcName)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func latestBackup(dir, srcName string) (string, error) {
	names := listBackups(dir, srcName)
	if len(names) == 0 {
		return "", errors.New("没有可还原的备份")
	}
	return filepath.Join(dir, names[len(names)-1]), nil
}

// ---------- 小工具 ----------

func strOf(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func numClaim(m map[string]any, k string) int64 {
	f, _ := m[k].(float64)
	return int64(f)
}

// jwtClaims 只解码 JWT 载荷（不验签），用于读取 exp / iat / sid。
func jwtClaims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// cloneMap 经 JSON 往返做深拷贝；nil 返回空 map。
func cloneMap(m map[string]any) map[string]any {
	out := map[string]any{}
	if len(m) == 0 {
		return out
	}
	b, err := json.Marshal(m)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}
