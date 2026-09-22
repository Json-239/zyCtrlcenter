//go:build !windows

package process

import "syscall"

// hideWindowAttr 非 Windows 平台无控制台窗口概念。
func hideWindowAttr() *syscall.SysProcAttr {
	return nil
}
