package auth

import "testing"

func TestSafeUID(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../x", `a\b`, "a/b", "c:x", "a\x00"} {
		if safeUID(bad) == nil {
			t.Errorf("uid %q should be rejected", bad)
		}
	}
	for _, ok := range []string{"a1b2c3d4", "user_01-abc", "x.y@z"} {
		if err := safeUID(ok); err != nil {
			t.Errorf("uid %q rejected: %v", ok, err)
		}
	}
}
