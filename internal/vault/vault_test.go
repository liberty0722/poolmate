package vault

import "testing"

func TestRoundTrip(t *testing.T) {
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
}
