//go:build linux

package main

import "syscall"

// freeGB 返回目录所在文件系统的可用空间（GB）。
func freeGB(path string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	// Bavail 为普通用户可用块数
	bytes := st.Bavail * uint64(st.Bsize)
	return int64(bytes / (1 << 30)), true
}
