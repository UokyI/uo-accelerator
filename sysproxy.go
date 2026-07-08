package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// ProxyBackup 保存/恢复的代理配置
type ProxyBackup struct {
	SystemProxyEnabled  bool   `json:"system_proxy_enabled"`
	SystemProxyServer   string `json:"system_proxy_server"`
	SystemProxyOverride string `json:"system_proxy_override"`
	GitHTTPProxy        string `json:"git_http_proxy"`
	GitHTTPSProxy       string `json:"git_https_proxy"`
}

const proxyBackupFile = "proxy-backup.json"
const proxyServerAddr = "127.0.0.1:9910"

// SysProxyManager Windows 系统代理管理器
type SysProxyManager struct {
	backupDir string
}

// NewSysProxyManager 创建系统代理管理器
func NewSysProxyManager(backupDir string) *SysProxyManager {
	os.MkdirAll(backupDir, 0700)
	return &SysProxyManager{backupDir: backupDir}
}

// GetStatus 获取当前代理状态
func (m *SysProxyManager) GetStatus() map[string]interface{} {
	status := map[string]interface{}{
		"system_proxy_enabled":  false,
		"system_proxy_server":   "",
		"system_proxy_override": "",
		"git_http_proxy":        "",
		"git_https_proxy":       "",
		"saved_backup":          false,
		"accelerator_running":   true,
	}

	// 读 Windows 系统代理
	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`,
		registry.QUERY_VALUE)
	if err == nil {
		defer key.Close()

		if enable, _, err := key.GetIntegerValue("ProxyEnable"); err == nil {
			status["system_proxy_enabled"] = enable == 1
		}
		if server, _, err := key.GetStringValue("ProxyServer"); err == nil {
			status["system_proxy_server"] = server
		}
		if override, _, err := key.GetStringValue("ProxyOverride"); err == nil {
			status["system_proxy_override"] = override
		}
	}

	// 读 Git 代理
	status["git_http_proxy"] = runGitConfig("http.proxy")
	status["git_https_proxy"] = runGitConfig("https.proxy")

	// 检查是否有备份
	backupPath := filepath.Join(m.backupDir, proxyBackupFile)
	if _, err := os.Stat(backupPath); err == nil {
		status["saved_backup"] = true
	}

	return status
}

// Enable 开启代理（保存旧配置 → 设置新配置）
func (m *SysProxyManager) Enable() error {
	// 1. 保存当前配置
	backup := m.captureCurrent()
	if err := m.saveBackup(backup); err != nil {
		return fmt.Errorf("保存备份失败: %w", err)
	}

	// 2. 设置系统代理
	Logf("[sysproxy] 正在设置系统代理: %s", proxyServerAddr)
	if err := m.setSystemProxy(true, proxyServerAddr, "<local>"); err != nil {
		return fmt.Errorf("设置系统代理失败: %w", err)
	}

	// 3. 设置 Git 代理
	runGitConfigSet("http.proxy", "http://"+proxyServerAddr)
	runGitConfigSet("https.proxy", "http://"+proxyServerAddr)

	Logf("[sysproxy] ✅ 系统代理已开启")
	return nil
}

// Disable 关闭代理（恢复旧配置）
func (m *SysProxyManager) Disable() error {
	backup, err := m.loadBackup()
	if err != nil {
		// 没有备份：直接关闭
		Logf("[sysproxy] 正在关闭系统代理（无备份）")
		m.setSystemProxy(false, "", "")
		runGitConfigUnset("http.proxy")
		runGitConfigUnset("https.proxy")
		Logf("[sysproxy] ✅ 系统代理已关闭")
		return nil
	}

	// 恢复系统代理
	Logf("[sysproxy] 正在恢复系统代理: %s (enabled=%v)", backup.SystemProxyServer, backup.SystemProxyEnabled)
	m.setSystemProxy(backup.SystemProxyEnabled, backup.SystemProxyServer, backup.SystemProxyOverride)

	// 恢复 Git 代理
	if backup.GitHTTPProxy != "" {
		runGitConfigSet("http.proxy", backup.GitHTTPProxy)
	} else {
		runGitConfigUnset("http.proxy")
	}
	if backup.GitHTTPSProxy != "" {
		runGitConfigSet("https.proxy", backup.GitHTTPSProxy)
	} else {
		runGitConfigUnset("https.proxy")
	}

	// 删除备份文件
	os.Remove(filepath.Join(m.backupDir, proxyBackupFile))
	return nil
}

// ========== 内部方法 ==========

func (m *SysProxyManager) captureCurrent() ProxyBackup {
	var b ProxyBackup

	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`,
		registry.QUERY_VALUE)
	if err == nil {
		defer key.Close()
		if enable, _, err := key.GetIntegerValue("ProxyEnable"); err == nil {
			b.SystemProxyEnabled = enable == 1
		}
		if server, _, err := key.GetStringValue("ProxyServer"); err == nil {
			b.SystemProxyServer = server
		}
		if override, _, err := key.GetStringValue("ProxyOverride"); err == nil {
			b.SystemProxyOverride = override
		}
	}

	b.GitHTTPProxy = runGitConfig("http.proxy")
	b.GitHTTPSProxy = runGitConfig("https.proxy")
	return b
}

func (m *SysProxyManager) saveBackup(b ProxyBackup) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.backupDir, proxyBackupFile), data, 0644)
}

func (m *SysProxyManager) loadBackup() (ProxyBackup, error) {
	var b ProxyBackup
	data, err := os.ReadFile(filepath.Join(m.backupDir, proxyBackupFile))
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return b, err
	}
	return b, nil
}

func (m *SysProxyManager) setSystemProxy(enable bool, server string, override string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`,
		registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()

	enableVal := uint32(0)
	if enable {
		enableVal = 1
	}
	if err := key.SetDWordValue("ProxyEnable", enableVal); err != nil {
		return err
	}
	if err := key.SetStringValue("ProxyServer", server); err != nil {
		return err
	}
	if override != "" {
		key.SetStringValue("ProxyOverride", override)
	} else {
		// 清空 ProxyOverride
		key.SetStringValue("ProxyOverride", "")
	}

	// 通知 Windows + 所有运行中的浏览器：代理已变更
	refreshSystemProxy()

	return nil
}

// refreshSystemProxy 静默通知系统代理设置已变更（不弹窗）
func refreshSystemProxy() {
	// 方式1：通知 WinINET 应用
	wininet := syscall.NewLazyDLL("wininet.dll")
	proc := wininet.NewProc("InternetSetOptionW")
	proc.Call(0, 39, 0, 0) // INTERNET_OPTION_SETTINGS_CHANGED
	proc.Call(0, 37, 0, 0) // INTERNET_OPTION_REFRESH

	// 方式2：广播 WM_SETTINGCHANGE，通知所有运行中的窗口代理已变
	user32 := syscall.NewLazyDLL("user32.dll")
	sendMsg := user32.NewProc("SendMessageTimeoutW")
	env, _ := syscall.UTF16PtrFromString("Environment")
	const HWND_BROADCAST = 0xFFFF
	const WM_SETTINGCHANGE = 0x001A
	const SMTO_ABORTIFHUNG = 0x0002
	sendMsg.Call(HWND_BROADCAST, WM_SETTINGCHANGE, 0, uintptr(unsafe.Pointer(env)), SMTO_ABORTIFHUNG, 3000, 0)

	Logf("[proxy] 系统代理已刷新: 写入注册表 + 通知所有窗口")
}

// runGitConfig 读取 git 配置
func runGitConfig(key string) string {
	cmd := runHidden("git", "config", "--global", key)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runGitConfigSet 设置 git 配置
func runGitConfigSet(key, value string) {
	runHidden("git", "config", "--global", key, value).Run()
}

// runGitConfigUnset 删除 git 配置
func runGitConfigUnset(key string) {
	runHidden("git", "config", "--global", "--unset", key).Run()
}
