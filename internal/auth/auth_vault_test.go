package auth

import (
	"os"
	"path/filepath"
	"testing"

	"poolmate/internal/vault"
)

func TestRewriteBetweenForms(t *testing.T) {
	if vault.MachineFactor() == "" {
		t.Skip("no machine id")
	}
	dir := t.TempDir()
	a := &Auth{UID: "u-1234567890", RefreshToken: "rt", AccessToken: "at"}
	if err := Save(dir, a, false, ""); err != nil {
		t.Fatal(err)
	}
	if !NeedsRewrite(dir, true) || NeedsRewrite(dir, false) {
		t.Fatal("plain file should need rewrite only when encryption is on")
	}
	if err := Rewrite(dir, a, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-u-1234567890.json")); !os.IsNotExist(err) {
		t.Fatal("plaintext file must be removed after encrypting")
	}
	if NeedsRewrite(dir, true) || !NeedsRewrite(dir, false) {
		t.Fatal("encrypted file state wrong")
	}
	got, errs := LoadDir(dir, "")
	if len(errs) > 0 || got["u-1234567890"] == nil || got["u-1234567890"].RefreshToken != "rt" {
		t.Fatalf("load encrypted: %v %v", got, errs)
	}
	// 设主密码后：机器因子加密的旧文件仍可读（回退无密码）
	got, errs = LoadDir(dir, "master")
	if len(errs) > 0 || got["u-1234567890"] == nil {
		t.Fatalf("load with master password fallback: %v", errs)
	}
	// 关闭加密：还原为明文
	if err := Rewrite(dir, a, false, ""); err != nil {
		t.Fatal(err)
	}
	if NeedsRewrite(dir, false) {
		t.Fatal("encrypted file should be gone after decrypting")
	}
}
