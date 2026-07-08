package main

import (
	"strings"
	"sync"
)

// PrefixRouter 前缀路由器（线程安全）
type PrefixRouter struct {
	mu    sync.RWMutex
	rules map[string]string // domain → prefix
	// 预设规则模板（用于重新启用）
	templates map[string]string
}

// NewPrefixRouter 创建前缀路由器
func NewPrefixRouter(rules map[string]string) *PrefixRouter {
	normalized := make(map[string]string)
	templates := make(map[string]string)
	for domain, prefix := range rules {
		prefix = strings.TrimSuffix(prefix, "/")
		if prefix != "" {
			domain = strings.ToLower(domain)
			normalized[domain] = prefix
			templates[domain] = prefix
		}
	}
	return &PrefixRouter{rules: normalized, templates: templates}
}

// Match 检查域名是否匹配前缀规则
func (r *PrefixRouter) Match(host string) (string, bool) {
	if h, _, err := splitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	r.mu.RLock()
	defer r.mu.RUnlock()
	prefix, ok := r.rules[host]
	return prefix, ok
}

// HasRule 检查是否有前缀规则
func (r *PrefixRouter) HasRule(host string) bool {
	if h, _, err := splitHostPort(host); err == nil {
		host = h
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.rules[strings.ToLower(host)]
	return ok
}

// RewriteRequestURL 改写请求 URL
func (r *PrefixRouter) RewriteRequestURL(scheme, host, path string) (string, bool) {
	if h, _, err := splitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	r.mu.RLock()
	defer r.mu.RUnlock()
	prefix, ok := r.rules[host]
	if !ok {
		return "", false
	}
	return prefix + "/" + scheme + "://" + host + path, true
}

// IsAnyEnabled 是否有任何启用中的规则
func (r *PrefixRouter) IsAnyEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.rules) > 0
}

// EnableAll 启用所有预设前缀规则
func (r *PrefixRouter) EnableAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = make(map[string]string)
	for domain, prefix := range r.templates {
		r.rules[domain] = prefix
	}
}

// DisableAll 禁用所有前缀规则（清空）
func (r *PrefixRouter) DisableAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = make(map[string]string)
}

// splitHostPort 分离主机和端口
func splitHostPort(hostPort string) (host string, port string, err error) {
	// 处理 IPv6
	if strings.HasPrefix(hostPort, "[") {
		if idx := strings.LastIndex(hostPort, "]"); idx >= 0 {
			host = hostPort[1:idx]
			if idx+1 < len(hostPort) && hostPort[idx+1] == ':' {
				port = hostPort[idx+2:]
			}
			return
		}
	}

	if idx := strings.LastIndex(hostPort, ":"); idx >= 0 {
		host = hostPort[:idx]
		port = hostPort[idx+1:]
		return
	}

	return hostPort, "", nil
}

// AllRules 返回所有规则（用于显示）
func (r *PrefixRouter) AllRules() map[string]string {
	return r.rules
}
