package vault

import (
	"encoding/json"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	if MachineFactor() == "" {
		t.Skip("no machine id on this host")
	}
	ct, err := Encrypt([]byte("secret"), "pw")
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(ct) {
		t.Fatal("not recognized as vault blob")
	}
	pt, err := Decrypt(ct, "pw")
	if err != nil || string(pt) != "secret" {
		t.Fatalf("decrypt: %q %v", pt, err)
	}
	if _, err := Decrypt(ct, "wrong"); err == nil {
		t.Fatal("wrong password accepted")
	}
	// 无主密码也能用（仅机器绑定）
	ct2, err := Encrypt([]byte("x"), "")
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := Decrypt(ct2, ""); err != nil || string(pt) != "x" {
		t.Fatalf("machine-only decrypt: %q %v", pt, err)
	}
}

func TestPortableIgnoresMachine(t *testing.T) {
	ct, err := EncryptPortable([]byte("bundle"), "pw")
	if err != nil {
		t.Fatal(err)
	}
	var b blob
	_ = json.Unmarshal(ct, &b)
	if b.Bind != bindPassword {
		t.Fatalf("bind = %q", b.Bind)
	}
	// 模拟换机器：机器因子不同也能解开
	saved := machineVal
	machineVal = "another-machine"
	defer func() { machineVal = saved }()
	if pt, err := Decrypt(ct, "pw"); err != nil || string(pt) != "bundle" {
		t.Fatalf("portable decrypt on other machine: %q %v", pt, err)
	}
	if _, err := Decrypt(ct, "nope"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if _, err := EncryptPortable([]byte("x"), ""); err == nil {
		t.Fatal("portable encryption without password must fail")
	}
}

func TestLegacyBlobStillReadable(t *testing.T) {
	ct, err := seal([]byte("old"), "pw", bindLegacy)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := Decrypt(ct, "pw"); err != nil || string(pt) != "old" {
		t.Fatalf("legacy decrypt: %q %v", pt, err)
	}
}
