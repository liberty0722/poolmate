//go:build windows

package vault

import "golang.org/x/sys/windows/registry"

// windowsMachineGUID 读 HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid（无需管理员，重装系统前不变）。
// 显式用 64 位视图，避免 32 位进程被重定向到 WOW6432Node。
func windowsMachineGUID() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	return v
}
