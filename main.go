package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var probing int32

func main() {
	configPath := flag.String("c", "", "配置文件路径（默认 exe 所在目录下的 config.yaml）")
	flag.Parse()

	// 默认在 exe 自身所在目录找 config.yaml，支持双击启动
	if *configPath == "" {
		exeDir, _ := os.Executable()
		if exePath, err := os.Readlink(exeDir); err == nil {
			exeDir = exePath
		}
		defaultCfg := filepath.Join(filepath.Dir(exeDir), "config.yaml")
		configPath = &defaultCfg
	}

	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║        GitHub Accelerator v1.0               ║")
	fmt.Println("║        本地智能代理 + 镜像前缀加速             ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[error] %v\n", err)
		pauseExit(1)
	}
	fmt.Printf("[config] 配置加载成功 (监听 %s)\n", cfg.Listen)

	resolver := NewResolver(cfg.DoH, ParseDuration(cfg.ProbeTimeout, 3*time.Second))
	prefixRouter := NewPrefixRouter(cfg.Prefixes)
	prober := NewProber(ParseDuration(cfg.ProbeTimeout, 3*time.Second), cfg.ProbePaths)
	routeTable := NewRouteTable()

	// 清理端口 + 预加载路由
	killPort(strings.TrimLeft(cfg.Listen, ":"))
	seedRouteTable(routeTable, cfg.Domains)

	// 启动代理
	proxy := NewProxyServer(cfg.Listen, routeTable, prefixRouter)
	go func() {
		if err := proxy.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "[proxy] 启动失败: %v\n", err)
			pauseExit(1)
		}
	}()
	time.Sleep(300 * time.Millisecond)

	// 同步探测
	fmt.Println("\n[probe] 开始首次探测...")
	runProbe(resolver, prober, routeTable, cfg)

	// 定时探测
	probeInterval := ParseDuration(cfg.ProbeInterval, 5*time.Minute)
	go func() {
		ticker := time.NewTicker(probeInterval)
		defer ticker.Stop()
		for range ticker.C {
			fmt.Println("\n[probe] 定时重新探测...")
			runProbe(resolver, prober, routeTable, cfg)
		}
	}()

	fmt.Println("\n[proxy] 🚀 代理已启动，3秒后自动开启系统代理...")
	time.Sleep(3 * time.Second)

	sysMgr := NewSysProxyManager("./certs")
	if err := sysMgr.Enable(); err != nil {
		fmt.Printf("[proxy] ⚠️ 自动设置代理失败: %v\n", err)
		fmt.Println("[proxy]    请手动设置系统代理: 127.0.0.1:9910")
	} else {
		fmt.Println("[proxy] ✅ 系统代理已自动开启 (127.0.0.1:9910)")
	}
	fmt.Println("[proxy]    Ctrl+C 退出并自动恢复代理")

	// Ctrl+C 退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Println("\n[proxy] 正在清理代理设置...")
	sysMgr.Disable()
	fmt.Println("[proxy] 已退出")
}

func runProbe(r *Resolver, p *Prober, rt *RouteTable, cfg *Config) {
	atomic.StoreInt32(&probing, 1)
	defer atomic.StoreInt32(&probing, 0)

	fmt.Println("[probe] Step 1/2: DoH DNS 解析...")
	resolved := r.ResolveAll(cfg.Domains)
	addFallbackIPs(resolved)
	for _, d := range cfg.Domains {
		if _, ok := resolved[d]; !ok {
			resolved[d] = nil
		}
	}
	addFallbackIPs(resolved)
	for domain, ips := range resolved {
		rt.SetFallback(domain, ips)
	}
	fmt.Printf("[probe] 解析完成: %d 个域名, 共 %d 个候选 IP\n", len(resolved), countIPs(resolved))

	fmt.Println("[probe] Step 2/2: TCP + HTTPS 并发测速...")
	probed := p.ProbeAll(resolved, cfg.TopN)
	rt.Update(probed)
	PrintProbeResults(probed)
}

func countIPs(m map[string][]string) int {
	n := 0
	for _, v := range m { n += len(v) }
	return n
}

// addFallbackIPs 为 GitHub 域名添加兜底 IP
func addFallbackIPs(resolved map[string][]string) {
	// Fastly CDN IP 段 — githubusercontent 家族
	fastlyIPs := []string{
		"185.199.108.133", "185.199.109.133", "185.199.110.133", "185.199.111.133",
	}

	// Fastly CDN IP 段 — githubassets / collector（静态资源、埋点）
	fastlyAssetsIPs := []string{
		"185.199.108.154", "185.199.109.154", "185.199.110.154", "185.199.111.154",
	}

	// GitHub 主站常见 IP（源站，非 CDN）
	githubIPs := []string{
		"20.27.177.113", "20.205.243.166",
		"140.82.113.3", "140.82.114.3", "140.82.112.3", "140.82.112.4",
		"140.82.113.4", "140.82.121.3",
	}

	for domain, ips := range resolved {
		// githubusercontent.com 家族 → Fastly CDN (.133)
		if contains(domain, "githubusercontent.com") {
			for _, fip := range fastlyIPs {
				if !containsSlice(ips, fip) {
					resolved[domain] = append(resolved[domain], fip)
				}
			}
		}

		// githubassets / collector → Fastly CDN (.154)
		if domain == "github.githubassets.com" || domain == "collector.github.com" {
			for _, fip := range fastlyAssetsIPs {
				if !containsSlice(ips, fip) {
					resolved[domain] = append(resolved[domain], fip)
				}
			}
		}

		// github.com / api / codeload / gist → GitHub 源站 IP
		if domain == "github.com" || domain == "api.github.com" ||
			domain == "codeload.github.com" || domain == "gist.github.com" {
			for _, gip := range githubIPs {
				if !containsSlice(ips, gip) {
					resolved[domain] = append(resolved[domain], gip)
				}
			}
		}
	}

	// 兜底：空条目补默认 IP
	for domain, ips := range resolved {
		if len(ips) == 0 {
			if contains(domain, "githubusercontent.com") {
				resolved[domain] = fastlyIPs
			} else if domain == "github.githubassets.com" || domain == "collector.github.com" {
				resolved[domain] = fastlyAssetsIPs
			} else {
				resolved[domain] = githubIPs
			}
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && s[len(s)-len(substr):] == substr
}

func containsSlice(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// killPort 杀掉占用指定端口的进程（解决上次未正常退出的残留）
func killPort(port string) {
	out, err := runHidden("netstat", "-ano").Output()
	if err != nil {
		return
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// 匹配 LISTENING 状态 + 目标端口
		if fields[3] != "LISTENING" {
			continue
		}
		addr := fields[1]
		if !strings.HasSuffix(addr, ":"+port) {
			continue
		}
		pid := fields[4]
		Logf("[startup] 端口 %s 被 PID %s 占用，正在清理...", port, pid)
		// 只杀自己的旧进程（exe 名包含 github-accelerator）
		taskOut, _ := runHidden("tasklist", "/FI", "PID eq "+pid, "/FO", "CSV", "/NH").Output()
		if strings.Contains(strings.ToLower(string(taskOut)), "github-accelerator") {
			runHidden("taskkill", "/F", "/PID", pid).Run()
			Logf("[startup] 已终止旧进程 PID %s", pid)
			time.Sleep(500 * time.Millisecond)
		}
		return
	}
	// 通用兜底：直接尝试按 PID 杀
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[3] != "LISTENING" || !strings.HasSuffix(fields[1], ":"+port) {
			continue
		}
		runHidden("taskkill", "/F", "/PID", fields[4]).Run()
		Logf("[startup] 已强制清理端口 %s (PID %s)", port, fields[4])
		time.Sleep(500 * time.Millisecond)
		return
	}
}

// runHidden 运行命令（GUI 模式隐藏子进程窗口，避免闪烁）
func runHidden(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

// seedRouteTable 提前注入兜底 IP，确保代理启动后立即可用
func seedRouteTable(rt *RouteTable, domains []string) {
	fastlyIPs := []string{"185.199.108.133", "185.199.109.133", "185.199.110.133", "185.199.111.133"}
	fastlyAssetsIPs := []string{"185.199.108.154", "185.199.109.154", "185.199.110.154", "185.199.111.154"}
	// 最优 IP 排前面（20.27.177.113 国内实测最快）
	githubIPs := []string{"20.27.177.113", "20.205.243.166",
		"140.82.113.3", "140.82.114.3", "140.82.112.3", "140.82.112.4",
		"140.82.113.4", "140.82.121.3"}

	for _, domain := range domains {
		switch {
		case contains(domain, "githubusercontent.com"):
			rt.SetFallback(domain, fastlyIPs)
		case domain == "github.githubassets.com" || domain == "collector.github.com":
			rt.SetFallback(domain, fastlyAssetsIPs)
		default:
			rt.SetFallback(domain, githubIPs)
		}
	}
	Logf("[startup] 已为 %d 个域名预加载兜底路由", len(domains))
}

// pauseExit 打印错误并等待按键后退出（双击 exe 时能看到错误信息）
func pauseExit(code int) {
	fmt.Fprintf(os.Stderr, "\n[error] 按 Enter 键退出...")
	fmt.Scanln()
	os.Exit(code)
}
