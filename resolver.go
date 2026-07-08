package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// DNSAnswer DoH 响应中的 Answer 条目
type DNSAnswer struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	TTL  int    `json:"TTL"`
	Data string `json:"data"`
}

// DoHResponse DoH JSON 响应
type DoHResponse struct {
	Status int         `json:"Status"`
	Answer []DNSAnswer `json:"Answer,omitempty"`
}

// Resolver DNS 解析器
type Resolver struct {
	dohURLs   []string
	client    *http.Client
	cache     map[string][]string // domain → IPs
	cacheMu   sync.RWMutex
	cacheTTL  time.Duration
	cacheTime map[string]time.Time
}

// NewResolver 创建 DNS 解析器
func NewResolver(dohURLs []string, timeout time.Duration) *Resolver {
	return &Resolver{
		dohURLs: dohURLs,
		client: &http.Client{
			Timeout: timeout,
		},
		cache:     make(map[string][]string),
		cacheTTL:  10 * time.Minute,
		cacheTime: make(map[string]time.Time),
	}
}

// Resolve 解析域名的 A 记录（带缓存）
func (r *Resolver) Resolve(domain string) ([]string, error) {
	// 先查缓存
	r.cacheMu.RLock()
	if ips, ok := r.cache[domain]; ok {
		if time.Since(r.cacheTime[domain]) < r.cacheTTL {
			r.cacheMu.RUnlock()
			return ips, nil
		}
	}
	r.cacheMu.RUnlock()

	// 通过 DoH 查询
	ips, err := r.resolveDoH(domain)
	if err != nil {
		// 查询失败，返回缓存（即使过期）
		r.cacheMu.RLock()
		if cached, ok := r.cache[domain]; ok {
			r.cacheMu.RUnlock()
			return cached, nil
		}
		r.cacheMu.RUnlock()
		return nil, err
	}

	// 更新缓存
	r.cacheMu.Lock()
	r.cache[domain] = ips
	r.cacheTime[domain] = time.Now()
	r.cacheMu.Unlock()

	return ips, nil
}

// resolveDoH 通过 DoH 查询 A 记录
func (r *Resolver) resolveDoH(domain string) ([]string, error) {
	seen := make(map[string]bool)
	var allIPs []string

	for _, dohURL := range r.dohURLs {
		url := fmt.Sprintf("%s?name=%s&type=A", dohURL, domain)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/dns-json")

		resp, err := r.client.Do(req)
		if err != nil {
			continue
		}

		var dohResp DoHResponse
		if err := json.NewDecoder(resp.Body).Decode(&dohResp); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		for _, ans := range dohResp.Answer {
			if ans.Type == 1 && isValidIPv4(ans.Data) { // A 记录
				if !seen[ans.Data] {
					seen[ans.Data] = true
					allIPs = append(allIPs, ans.Data)
				}
			}
		}
	}

	if len(allIPs) == 0 {
		return nil, fmt.Errorf("无法解析域名 %s: 所有 DoH 端点均无响应", domain)
	}

	return allIPs, nil
}

// ResolveAll 并发解析所有域名
func (r *Resolver) ResolveAll(domains []string) map[string][]string {
	result := make(map[string][]string)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, d := range domains {
		wg.Add(1)
		go func(domain string) {
			defer wg.Done()
			ips, err := r.Resolve(domain)
			if err == nil && len(ips) > 0 {
				mu.Lock()
				result[domain] = ips
				mu.Unlock()
			}
		}(d)
	}

	wg.Wait()
	return result
}

// isValidIPv4 检查是否是有效的 IPv4 地址
func isValidIPv4(s string) bool {
	parts := 0
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			if n > 255 {
				return false
			}
			parts++
			n = 0
		} else if s[i] >= '0' && s[i] <= '9' {
			n = n*10 + int(s[i]-'0')
		} else {
			return false
		}
	}
	if n > 255 {
		return false
	}
	parts++
	return parts == 4
}
