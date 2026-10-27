// Package backup 凭证导出/导入与定时自动备份。
// 导出格式与 auths/*.json 同构的数组；可选 vault 加密（密码由用户给出）。
package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"poolmate/internal/auth"
	"poolmate/internal/vault"
)

// ExportBundle 把账号集合打包为一个 JSON 文档；password 非空则加密。
func ExportBundle(auths map[string]*auth.Auth, password string) ([]byte, error) {
	items := make([]json.RawMessage, 0, len(auths))
	for _, a := range auths {
		raw, err := a.Marshal(false, "")
		if err != nil {
			return nil, err
		}
		items = append(items, raw)
	}
	bundle, err := json.MarshalIndent(map[string]any{
		"format": "poolmate-backup-v1", "exported_at": time.Now().Format(time.RFC3339),
		"count": len(items), "accounts": items,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	if password != "" {
		return vault.Encrypt(bundle, password)
	}
	return bundle, nil
}

// ImportBundle 解析导出文件（自动识别明文/加密），同 uid 保留 token 较新者。
func ImportBundle(raw []byte, password string) (map[string]*auth.Auth, []string, error) {
	if vault.IsEncrypted(raw) {
		plain, err := vault.Decrypt(raw, password)
		if err != nil {
			return nil, nil, err
		}
		raw = plain
	}
	var skipped []string
	// 支持两种形态：完整导出文档 / 单个凭证文件 / 凭证数组
	var doc struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &doc); err == nil && len(doc.Accounts) > 0 {
		raws = doc.Accounts
	} else {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
			raws = arr
		} else {
			raws = []json.RawMessage{raw}
		}
	}
	out := map[string]*auth.Auth{}
	for _, r := range raws {
		a, err := auth.Parse(r, password)
		if err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		if old, ok := out[a.UID]; !ok || a.Snapshot().ExpiresAt > old.Snapshot().ExpiresAt {
			out[a.UID] = a
		} else {
			skipped = append(skipped, a.DisplayName()+"（已有更新凭证，跳过）")
		}
	}
	if len(out) == 0 && len(skipped) == 0 {
		return nil, nil, fmt.Errorf("文件中没有可识别的账号")
	}
	return out, skipped, nil
}

// AutoBackup 打包凭证目录与状态文件到 dest/20060102-150405/，保留 keep 份。
func AutoBackup(authDir, stateFile, dest string, keep int) (string, error) {
	stamp := time.Now().Format("20060102-150405")
	target := filepath.Join(dest, stamp)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return "", err
	}
	// 凭证：原样拷贝（密文形态拷贝密文，不接触明文）
	entries, err := os.ReadDir(authDir)
	if err == nil {
		ad := filepath.Join(target, "auths")
		_ = os.MkdirAll(ad, 0o700)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(authDir, e.Name()))
			if err != nil {
				continue
			}
			_ = os.WriteFile(filepath.Join(ad, e.Name()), raw, 0o600)
		}
	}
	// 状态
	if raw, err := os.ReadFile(stateFile); err == nil {
		_ = os.WriteFile(filepath.Join(target, "state.json"), raw, 0o600)
	}
	// 滚动清理
	keepN := keep
	if keepN <= 0 {
		keepN = 14
	}
	dirs, _ := os.ReadDir(dest)
	var names []string
	for _, d := range dirs {
		if d.IsDir() {
			names = append(names, d.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keepN {
		_ = os.RemoveAll(filepath.Join(dest, names[0]))
		names = names[1:]
	}
	return target, nil
}

// ListBackups 列出已有备份（新→旧）。
func ListBackups(dest string) []string {
	var out []string
	dirs, _ := os.ReadDir(dest)
	for _, d := range dirs {
		if d.IsDir() {
			out = append(out, d.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}
