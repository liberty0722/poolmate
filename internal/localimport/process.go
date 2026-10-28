package localimport

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// exeName 客户端主程序的进程映像名（Electron 应用的 productName + .exe）。
func exeName(src Source) string { return src.Name + ".exe" }

// ClientRunning 客户端是否正在运行（仅 Windows 有效，其他平台恒为 false）。
func ClientRunning(src Source) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+exeName(src), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), strings.ToLower(`"`+exeName(src)+`"`))
}

// CloseClient 关闭客户端：先请求正常退出，等待 wait 后仍未退出再强制结束。
// 返回是否确认已退出。
func CloseClient(src Source, wait time.Duration) error {
	if !ClientRunning(src) {
		return nil
	}
	_ = exec.Command("taskkill", "/IM", exeName(src), "/T").Run()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if !ClientRunning(src) {
			time.Sleep(800 * time.Millisecond) // 等文件句柄释放、WAL 落盘
			return nil
		}
	}
	_ = exec.Command("taskkill", "/IM", exeName(src), "/T", "/F").Run()
	time.Sleep(1500 * time.Millisecond)
	if ClientRunning(src) {
		return fmt.Errorf("无法结束 %s，请手动退出后重试", src.Name)
	}
	return nil
}
