// Package vault 提供凭证加密原语：AES-256-GCM + PBKDF2 密钥派生。
//
// 三种绑定方式（写在密文的 bind 字段里，解密时按它选择派生材料）：
//   - "m2"（Encrypt，凭证落盘用）：可选主密码 + 本机机器标识。机器标识取 Windows MachineGuid /
//     Linux machine-id，改电脑名、换 Windows 用户都不受影响；文件离开本机即无法解密。
//   - "pw"（EncryptPortable，导出/备份用）：只用密码，可在任意机器导入。
//   - ""（旧版本写出的密文）：密码 + 主板 UUID + 电脑名 + 用户名，仅用于兼容读取。
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
	"time"
)

const (
	version    = 1
	kdfIter    = 210_000
	saltLen    = 16
	nonceLen   = 12
	keyLen     = 32
	magicLabel = "poolmate-vault-v1"

	bindLegacy   = ""
	bindMachine  = "m2"
	bindPassword = "pw"
)

type blob struct {
	V     int    `json:"v"`
	KDF   string `json:"kdf"`
	Iter  int    `json:"iter"`
	Bind  string `json:"bind,omitempty"`
	Salt  string `json:"salt"`
	Nonce string `json:"nonce"`
	CT    string `json:"ct"`
}

// IsEncrypted 判断内容是否为 vault 密文。
func IsEncrypted(raw []byte) bool {
	var b blob
	return json.Unmarshal(raw, &b) == nil && b.V == version && b.KDF == "pbkdf2-sha256" && b.CT != ""
}

// IsLegacy 是否为旧版本（绑定电脑名/用户名）写出的密文，需要重新加密为新格式。
func IsLegacy(raw []byte) bool {
	var b blob
	return IsEncrypted(raw) && json.Unmarshal(raw, &b) == nil && b.Bind == bindLegacy
}

// Encrypt 用「可选主密码 + 本机机器标识」加密（凭证落盘），只能在本机解密。
func Encrypt(plain []byte, password string) ([]byte, error) {
	if MachineFactor() == "" {
		return nil, errors.New("读取不到本机机器标识，无法做机器绑定加密")
	}
	return seal(plain, password, bindMachine)
}

// EncryptPortable 只用密码加密（导出文件），换机器也能用同一密码导入。
func EncryptPortable(plain []byte, password string) ([]byte, error) {
	if password == "" {
		return nil, errors.New("可移植加密必须提供密码")
	}
	return seal(plain, password, bindPassword)
}

func seal(plain []byte, password, bind string) ([]byte, error) {
	salt := make([]byte, saltLen)
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key, err := derive(password, bind, salt, kdfIter)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(mustAES(key))
	if err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, plain, []byte(magicLabel))
	return json.Marshal(blob{
		V: version, KDF: "pbkdf2-sha256", Iter: kdfIter, Bind: bind,
		Salt:  base64.StdEncoding.EncodeToString(salt),
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		CT:    base64.StdEncoding.EncodeToString(ct),
	})
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
	// 按密文自带的迭代次数派生（兼容将来调整 kdfIter）；限定范围防恶意文件拖死 CPU。
	iter := b.Iter
	if iter <= 0 {
		iter = kdfIter
	}
	if iter < 10_000 || iter > 5_000_000 {
		return nil, fmt.Errorf("vault 密文迭代次数异常：%d", iter)
	}
	switch b.Bind {
	case bindLegacy, bindMachine, bindPassword:
	default:
		return nil, fmt.Errorf("不支持的 vault 绑定方式 %q（请升级 PoolMate）", b.Bind)
	}
	key, err := derive(password, b.Bind, salt, iter)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(mustAES(key))
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, ct, []byte(magicLabel))
	if err != nil {
		switch b.Bind {
		case bindPassword:
			return nil, errors.New("解密失败：密码错误或文件已损坏")
		case bindLegacy:
			return nil, errors.New("解密失败：密码错误，或该文件由旧版本在其他电脑/电脑名/用户下加密")
		}
		return nil, errors.New("解密失败：主密码错误、文件不是在本机加密的，或文件已损坏")
	}
	return plain, nil
}

func derive(password, bind string, salt []byte, iter int) ([]byte, error) {
	var material string
	switch bind {
	case bindPassword:
		material = password + "|portable|" + magicLabel
	case bindMachine:
		material = password + "|" + MachineFactor() + "|m2|" + magicLabel
	default:
		material = password + "|" + legacyFactor() + "|" + magicLabel
	}
	return pbkdf2.Key(sha256.New, material, salt, iter, keyLen)
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
	legacyOnce  sync.Once
	legacyVal   string
)

// MachineFactor 本机机器标识的摘要（不直接用作密钥，只参与派生）；取不到时为空。
// 只依赖系统安装时生成的机器 ID，与电脑名、登录用户无关。
func MachineFactor() string {
	machineOnce.Do(func() {
		id := machineID()
		if id == "" {
			return
		}
		sum := sha256.Sum256([]byte("poolmate-machine|" + strings.ToLower(id)))
		machineVal = base64.StdEncoding.EncodeToString(sum[:16])
	})
	return machineVal
}

func machineID() string {
	if runtime.GOOS == "windows" {
		if id := windowsMachineGUID(); id != "" {
			return id
		}
		return windowsProductUUID()
	}
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if raw, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(raw)); id != "" {
				return id
			}
		}
	}
	return ""
}

// windowsProductUUID 主板 UUID（Win32_ComputerSystemProduct）。开机自启时 WMI 可能尚未就绪，失败重试。
func windowsProductUUID() string {
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"(Get-CimInstance Win32_ComputerSystemProduct).UUID").Output()
		if err == nil {
			if id := strings.TrimSpace(string(out)); id != "" {
				return id
			}
		}
	}
	return ""
}

// legacyFactor 旧版本的机器因子（主板 UUID + 电脑名 + 用户名），只用于解密旧密文。
func legacyFactor() string {
	legacyOnce.Do(func() {
		id := ""
		if runtime.GOOS == "windows" {
			id = windowsProductUUID()
		} else {
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
		legacyVal = base64.StdEncoding.EncodeToString(sum[:16])
	})
	return legacyVal
}
