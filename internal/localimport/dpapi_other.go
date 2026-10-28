//go:build !windows

package localimport

import "errors"

func dpapiUnprotect(in []byte) ([]byte, error) {
	return nil, errors.New("本机客户端导入目前仅支持 Windows")
}
