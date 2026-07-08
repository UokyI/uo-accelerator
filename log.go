package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

var logWriter io.Writer

// InitLogger 创建日志文件，返回写入器
// 在 -H windowsgui 模式下，没有控制台，全靠日志文件
func InitLogger(logDir string) *os.File {
	os.MkdirAll(logDir, 0700)
	logPath := filepath.Join(logDir, "accelerator.log")

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil
	}

	fmt.Fprintf(f, "\n=== %s ===\n", time.Now().Format("2006-01-02 15:04:05"))

	// 同时写文件和控制台（控制台不存在也不报错）
	logWriter = io.MultiWriter(f, os.Stdout)
	log.SetOutput(logWriter)
	log.SetFlags(log.LstdFlags)

	return f
}

// Logf 格式化日志
func Logf(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	if logWriter != nil {
		fmt.Fprintln(logWriter, msg)
	} else {
		fmt.Println(msg)
	}
}

// FatalError GUI 模式下弹窗报错 + 写日志
func FatalError(msg string) {
	fmt.Fprintln(logWriter, "[FATAL]", msg)
	// Windows MessageBox
	user32 := syscall.NewLazyDLL("user32.dll")
	msgBox := user32.NewProc("MessageBoxW")
	title, _ := syscall.UTF16PtrFromString("GitHub Accelerator 错误")
	body, _ := syscall.UTF16PtrFromString(msg)
	msgBox.Call(0, uintptr(unsafe.Pointer(body)), uintptr(unsafe.Pointer(title)), 0x10)
	os.Exit(1)
}

