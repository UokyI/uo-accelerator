package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RouteTable 路由表（线程安全）
type RouteTable struct {
	mu       sync.RWMutex
	routes   map[string][]ProbeCandidate // domain → 排序后的候选 IP 列表
	fallback map[string][]string         // domain → 兜底 IP（即使探测全失败）
}

// NewRouteTable 创建路由表
func NewRouteTable() *RouteTable {
	return &RouteTable{
		routes:   make(map[string][]ProbeCandidate),
		fallback: make(map[string][]string),
	}
}

// SetFallback 设置域名的兜底 IP 列表
func (rt *RouteTable) SetFallback(domain string, ips []string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.fallback[domain] = ips
}

// Update 更新路由表
func (rt *RouteTable) Update(probed map[string][]ProbeCandidate) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for domain, candidates := range probed {
		if len(candidates) > 0 {
			rt.routes[domain] = candidates
		}
	}
}

// GetBest 获取域名的最优 IP（探测失败时返回兜底 IP）
func (rt *RouteTable) GetBest(domain string) (string, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	// 优先用探测结果
	candidates, ok := rt.routes[domain]
	if ok && len(candidates) > 0 {
		return candidates[0].IP, true
	}

	// 探测失败，用兜底 IP（取第一个）
	fallbackIPs, ok := rt.fallback[domain]
	if ok && len(fallbackIPs) > 0 {
		return fallbackIPs[0], true
	}

	return "", false
}

// GetAllCandidates 获取域名所有候选 IP（探测 + 兜底）
func (rt *RouteTable) GetAllCandidates(domain string) []string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	var all []string
	seen := make(map[string]bool)

	for _, c := range rt.routes[domain] {
		if !seen[c.IP] {
			seen[c.IP] = true
			all = append(all, c.IP)
		}
	}
	for _, ip := range rt.fallback[domain] {
		if !seen[ip] {
			seen[ip] = true
			all = append(all, ip)
		}
	}
	return all
}

// IsGitHubDomain 检查是否为 GitHub 加速域名
func (rt *RouteTable) IsGitHubDomain(host string) bool {
	if h, _, err := splitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	_, inRoutes := rt.routes[host]
	_, inFallback := rt.fallback[host]
	return inRoutes || inFallback
}

// ProxyServer HTTP/HTTPS 代理服务器
type ProxyServer struct {
	addr         string
	routeTable   *RouteTable
	prefixRouter *PrefixRouter
	server       *http.Server
	transport    *http.Transport
}

// NewProxyServer 创建代理服务器
func NewProxyServer(addr string, routeTable *RouteTable, prefixRouter *PrefixRouter) *ProxyServer {
	ps := &ProxyServer{
		addr:         addr,
		routeTable:   routeTable,
		prefixRouter: prefixRouter,
	}

	ps.transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false,
		},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	ps.server = &http.Server{
		Addr:         addr,
		Handler:      http.HandlerFunc(ps.handleRequest),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second, // 大文件下载需要较长写超时
		IdleTimeout:  120 * time.Second,
	}

	return ps
}

// Start 启动代理服务
func (ps *ProxyServer) Start() error {
	return ps.server.ListenAndServe()
}

// handleRequest 处理代理请求
func (ps *ProxyServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		fmt.Printf("[proxy] CONNECT %s → %s\n", r.Host, "路由中...")
		ps.handleConnect(w, r)
	} else {
		ps.handleHTTP(w, r)
	}
}

// handleConnect 处理 HTTPS CONNECT 隧道
func (ps *ProxyServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	hostPort := r.URL.Host
	if hostPort == "" {
		hostPort = r.Host
	}

	host, _, _ := splitHostPort(hostPort)

	// 检查是否需要前缀重写
	if ps.prefixRouter.HasRule(host) {
		http.Error(w, "前缀代理未启用", http.StatusNotImplemented)
		return
	}

	// 只有 GitHub 域名才走路由（多IP遍历），其余全部直连
	if ps.routeTable.IsGitHubDomain(host) {
		// 第一个 IP 用完整超时，后续快速失败（网络环境差时不必逐个等 10s）
		for i, ip := range ps.routeTable.GetAllCandidates(host) {
			timeout := 10 * time.Second
			if i > 0 {
				timeout = 3 * time.Second
			}
			targetAddr := net.JoinHostPort(ip, "443")
			fmt.Printf("[proxy] %s → 尝试 %s (%v)\n", host, ip, timeout)

			destConn, err := net.DialTimeout("tcp", targetAddr, timeout)
			if err != nil {
				fmt.Printf("[proxy] %s → %s 失败: %v\n", host, ip, err)
				continue
			}

			// 连接成功，直接复用，不 close 再重拨
			ps.tunnelConn(w, destConn)
			return
		}
		fmt.Printf("[proxy] %s → 所有IP均失败\n", host)
		http.Error(w, "Bad Gateway: all IPs failed", http.StatusBadGateway)
		return
	}

	// 非 GitHub 域名：直连
	fmt.Printf("[proxy] %s → 直连\n", host)
	destConn, err := net.DialTimeout("tcp", hostPort, 10*time.Second)
	if err != nil {
		http.Error(w, "Bad Gateway: "+err.Error(), http.StatusBadGateway)
		return
	}
	ps.tunnelConn(w, destConn)
}

// tunnelConn 将已建立的连接 Hijack 为 CONNECT 隧道（一次拨号即用，不浪费端口）
func (ps *ProxyServer) tunnelConn(w http.ResponseWriter, destConn net.Conn) {
	defer destConn.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持 Hijack", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "Hijack 失败: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer clientConn.Close()

	// 告诉客户端连接已建立
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// 双向数据转发
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(destConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, destConn)
		done <- struct{}{}
	}()
	<-done
}

// handleHTTP 处理普通 HTTP 请求
func (ps *ProxyServer) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// 规范化请求 URL：确保 Host 不为空
	if r.URL.Scheme == "" {
		r.URL.Scheme = "http"
	}
	if r.URL.Host == "" {
		r.URL.Host = r.Host
	}
	if r.URL.Host == "" {
		http.Error(w, "无法确定请求目标", http.StatusBadRequest)
		return
	}

	host := r.URL.Host

	// 检查前缀规则
	if rewrittenURL, ok := ps.prefixRouter.RewriteRequestURL(r.URL.Scheme, host, r.URL.Path); ok {
		ps.proxyRewrittenHTTP(w, rewrittenURL)
		return
	}

	// GitHub 域名走最优 IP
	if bestIP, ok := ps.routeTable.GetBest(host); ok {
		ps.proxyToBestIP(w, r, bestIP)
		return
	}

	// 其他：直接代理
	ps.proxyDirect(w, r)
}

// proxyRewrittenHTTP 前缀重写 HTTP 请求
func (ps *ProxyServer) proxyRewrittenHTTP(w http.ResponseWriter, rewrittenURL string) {
	resp, err := http.Get(rewrittenURL)
	if err != nil {
		http.Error(w, "代理请求失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// proxyToBestIP 将请求转发到最优 IP（正确设置 SNI）
func (ps *ProxyServer) proxyToBestIP(w http.ResponseWriter, r *http.Request, bestIP string) {
	host := r.URL.Host
	if host == "" {
		host = r.Host
	}

	// 创建自定义 transport，用 IP 直连但 SNI 正确
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName: host, // 正确 SNI
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// 忽略 addr 中的域名，直连最优 IP
			_, port, _ := net.SplitHostPort(addr)
			if port == "" {
				port = "443"
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(bestIP, port))
		},
	}

	// 修改请求 URL Host 用于转发
	outReq := r.Clone(r.Context())
	outReq.URL.Host = host + ":443"
	outReq.Host = host
	outReq.RequestURI = ""

	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "代理请求失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// proxyDirect 直接代理（非 GitHub 域名）
func (ps *ProxyServer) proxyDirect(w http.ResponseWriter, r *http.Request) {
	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""

	resp, err := ps.transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "代理请求失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
