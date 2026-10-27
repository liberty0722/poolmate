// Package vault 提供凭证加密原语：AES-256-GCM + PBKDF2 密钥派生。
// 密钥由「可选主密码 + 机器绑定因子」共同派生：
//   - 设了主密码：密码 + 机器因子，拷走文件+知道机器也无法离线爆破以外的还原；
//   - 未设主密码：仅机器因子，文件离开本机即无法解密（防拷贝泄露，不防本机入侵）。
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"sync"
)

const (
	version    = 1
	kdfIter    = 210_000
	saltLen    = 16
	nonceLen   = 12
	keyLen     = 32
	magicLabel = "poolmate-vault-v1"
)

type blob struct {
	V     int    `json:"v"`
	KDF   string `json:"kdf"`
	Iter  int    `json:"iter"`
	Salt  string `json:"salt"`
	Nonce string `json:"nonce"`
	CT    string `json:"ct"`
}

// IsEncrypted 判断内容是否为 vault 密文。
func IsEncrypted(raw []byte) bool {
	var b blob
	return json.Unmarshal(raw, &b) == nil && b.V == version && b.KDF == "pbkdf2-sha256" && b.CT != ""
}

// Encrypt 用 password 加密 plain，输出 JSON 形态的密文。
func Encrypt(plain []byte, password string) ([]byte, error) {
	salt := make([]byte, saltLen)
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key, err := derive(password, salt)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(mustAES(key))
	if err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, plain, []byte(magicLabel))
	out, err := json.Marshal(blob{
		V: version, KDF: "pbkdf2-sha256", Iter: kdfIter,
		Salt:  base64.StdEncoding.EncodeToString(salt),
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		CT:    base64.StdEncoding.EncodeToString(ct),
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Decrypt 解密 vault 密文；密码错误返回明确错误。
func Decrypt(raw []byte, password string) ([]byte, error) {
	var b blob
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, errors.New("不是 vault 密文")
	}
	salt, err := base64.StdEncoding.DecodeString(b.Salt)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.DecodeString(b.Nonce)
	if err != nil {
		return nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(b.CT)
	if err != nil {
		return nil, err
	}
	key, err := derive(password, salt)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(mustAES(key))
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, ct, []byte(magicLabel))
	if err != nil {
		return nil, errors.New("解密失败：密码错误或密文已损坏")
	}
	return plain, nil
}

func derive(password string, salt []byte) ([]byte, error) {
	material := password + "|" + MachineFactor() + "|" + magicLabel
	return pbkdf2.Key(sha256.New, material, salt, kdfIter, keyLen)
}

func mustAES(key []byte) cipher.Block {
	blk, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	return blk
}

var (
	machineOnce sync.Once
	machineVal  string
)

// MachineFactor 返回稳定的机器绑定因子（不直接用作密钥，只参与派生）。
func MachineFactor() string {
	machineOnce.Do(func() {
		machineVal = detectMachine()
	})
	return machineVal
}

func detectMachine() string {
	id := ""
	if runtime.GOOS == "windows" {
		// Win32_ComputerSystemProduct.UUID 不需要管理员权限，重装系统前稳定。
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"(Get-CimInstance Win32_ComputerSystemProduct).UUID").Output()
		if err == nil {
			id = strings.TrimSpace(string(out))
		}
	} else {
		// Linux 系机器 ID。
		for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			if raw, err := os.ReadFile(p); err == nil {
				id = strings.TrimSpace(string(raw))
				break
			}
		}
	}
	host, _ := os.Hostname()
	userName := ""
	if u, err := user.Current(); err == nil {
		userName = u.Username
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|poolmate", id, host, userName)))
	return base64.StdEncoding.EncodeToString(sum[:16])
}
