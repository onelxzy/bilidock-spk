//go:build !linux

package main

// freeGB 在非 Linux 平台不提供空间信息。
func freeGB(path string) (int64, bool) { return 0, false }
