//go:build !windows

package vault

func windowsMachineGUID() string { return "" }
