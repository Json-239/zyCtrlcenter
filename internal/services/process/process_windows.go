//go:build windows

package process

import "syscall"

// hideWindowAttr Windows 下隐藏子进程控制台窗口。
func hideWindowAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true}
}
