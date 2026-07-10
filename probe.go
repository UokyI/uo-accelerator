package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// ProbeResult 探测结果
type ProbeResult struct {
	Domain     string
	IP         string
	Latency    time.Duration // TCP 连接延迟
	Throughput float64       // KB/s（-1 表示未测试）
	Available  bool
	Error      string
}

// ProbeCandidate 候选 IP 评分
type ProbeCandidate struct {
	IP         string
	Latency    time.Duration
	Throughput float64
	Score      float64 // 综合评分（越低越好）
}

// Prober IP 探测器
type Prober struct {
	timeout    time.Duration
	probePaths []string
	client     *http.Client
}

// NewProber 创建探测器
func NewProber(timeout time.Duration, probePaths []string) *Prober {
	return &Prober{
		timeout:    timeout,
		probePaths: probePaths,
	}
}

// maxProbeConcurrency 探测最大并发数（避免挤占正常代理请求的端口）
const maxProbeConcurrency = 10

// ProbeIPs 并发探测所有候选 IP，返回按评分排序的结果
func (p *Prober) ProbeIPs(domain string, ips []string, topN int) []ProbeCandidate {
	if len(ips) == 0 {
		return nil
	}

	results := make(chan ProbeCandidate, len(ips))
	var wg sync.WaitGroup
	sema := make(chan struct{}, maxProbeConcurrency)

	for _, ip := range ips {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			sema <- struct{}{}        // 获取令牌
			defer func() { <-sema }() // 释放令牌

			candidate := p.probeSingle(domain, ip)
			if candidate != nil {
				results <- *candidate
			}
		}(ip)
	}

	wg.Wait()
	close(results)

	var candidates []ProbeCandidate
	for c := range results {
		candidates = append(candidates, c)
	}

	// 按评分排序（越低越好）
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score < candidates[j].Score
	})

	// 取前 topN
	if topN > 0 && len(candidates) > topN {
		candidates = candidates[:topN]
	}

	return candidates
}

// probeSingle 探测单个 IP
func (p *Prober) probeSingle(domain string, ip string) *ProbeCandidate {
	// 1. TCP 连接测延迟
	latency, err := p.tcpConnect(ip)
	if err != nil {
		return nil
	}

	// 2. HTTPS 请求测吞吐量
	throughput := p.httpsThroughput(domain, ip)

	// 综合评分: 延迟(ms) * 权重0.6 + (1000/吞吐量) * 权重0.4
	latencyScore := float64(latency.Milliseconds())
	throughputScore := 0.0
	if throughput > 0 {
		throughputScore = 1000.0 / throughput // 吞吐量越高，分数越低
	} else {
		throughputScore = latencyScore * 2 // 吞吐量测试失败，惩罚
	}

	score := latencyScore*0.6 + throughputScore*0.4

	return &ProbeCandidate{
		IP:         ip,
		Latency:    latency,
		Throughput: throughput,
		Score:      score,
	}
}

// tcpConnect TCP 连接测试，返回延迟
func (p *Prober) tcpConnect(ip string) (time.Duration, error) {
	addr := net.JoinHostPort(ip, "443")
	start := time.Now()

	conn, err := net.DialTimeout("tcp", addr, p.timeout)
	if err != nil {
		return 0, err
	}
	// 设置 Linger=0，关闭时发 RST 跳过 TIME_WAIT，不浪费临时端口
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetLinger(0)
	}
	conn.Close()

	return time.Since(start), nil
}

// httpsThroughput HTTPS 请求吞吐量测试
func (p *Prober) httpsThroughput(domain string, ip string) float64 {
	addr := net.JoinHostPort(ip, "443")

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName:         domain, // 正确的 SNI
			InsecureSkipVerify: true,   // 跳过证书校验（测速不需要严格验证）
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: p.timeout}
			conn, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			// 关闭时发 RST 跳过 TIME_WAIT
			if tcp, ok := conn.(*net.TCPConn); ok {
				tcp.SetLinger(0)
			}
			return conn, nil
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   p.timeout,
	}

	var bestThroughput float64 = -1

	for _, path := range p.probePaths {
		url := fmt.Sprintf("https://%s%s", domain, path)
		// 注意：这里我们用 IP 连接但 Host 头设域名
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			continue
		}
		req.Host = domain

		// 用自定义 transport 并替换 URL host 为 IP
		req.URL.Host = addr

		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			continue
		}

		// 读取响应体，测量吞吐量
		buf := make([]byte, 32*1024)
		totalBytes := 0
		for {
			n, err := resp.Body.Read(buf)
			totalBytes += n
			if err != nil {
				break
			}
		}
		resp.Body.Close()

		elapsed := time.Since(start).Seconds()
		if elapsed > 0 && totalBytes > 0 {
			throughput := float64(totalBytes) / 1024.0 / elapsed // KB/s
			if throughput > bestThroughput {
				bestThroughput = throughput
			}
		}
	}

	return bestThroughput
}

// maxDomainProbeConcurrency 同时探测的最大域名数
const maxDomainProbeConcurrency = 3

// ProbeAll 并发探测所有域名的 IP（限制并发避免挤占正常代理端口）
func (p *Prober) ProbeAll(resolved map[string][]string, topN int) map[string][]ProbeCandidate {
	result := make(map[string][]ProbeCandidate)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sema := make(chan struct{}, maxDomainProbeConcurrency)

	for domain, ips := range resolved {
		wg.Add(1)
		go func(d string, ipList []string) {
			defer wg.Done()
			sema <- struct{}{}        // 获取令牌
			defer func() { <-sema }() // 释放令牌

			candidates := p.ProbeIPs(d, ipList, topN)
			if len(candidates) > 0 {
				mu.Lock()
				result[d] = candidates
				mu.Unlock()
			}
		}(domain, ips)
	}

	wg.Wait()
	return result
}

// PrintProbeResults 打印探测结果
func PrintProbeResults(probed map[string][]ProbeCandidate) {
	fmt.Println("\n========== 探测结果 ==========")
	for domain, candidates := range probed {
		if len(candidates) == 0 {
			fmt.Printf("  %-40s ❌ 无可用 IP\n", domain)
			continue
		}
		best := candidates[0]
		fmt.Printf("  %-40s ✅ %-16s 延迟: %4dms  吞吐: %7.1f KB/s  评分: %.1f\n",
			domain, best.IP, best.Latency.Milliseconds(), best.Throughput, best.Score)
	}
	fmt.Print("==============================\n\n")
}
