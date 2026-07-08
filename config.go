package main

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 应用配置
type Config struct {
	Listen        string            `yaml:"listen"`
	DoH           []string          `yaml:"doh"`
	Domains       []string          `yaml:"domains"`
	Prefixes      map[string]string `yaml:"prefixes"`
	ProbePaths    []string          `yaml:"probe_paths"`
	ProbeInterval string            `yaml:"probe_interval"`
	ProbeTimeout  string            `yaml:"probe_timeout"`
	TopN          int               `yaml:"top_n"`
	CertDir       string            `yaml:"cert_dir"`
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	return &Config{
		Listen: ":9910",
		DoH: []string{
			"https://dns.alidns.com/resolve",
			"https://doh.pub/dns-query",
		},
		Domains: []string{
			"github.com",
			"api.github.com",
			"codeload.github.com",
			"gist.github.com",
			"raw.githubusercontent.com",
			"objects.githubusercontent.com",
			"release-assets.githubusercontent.com",
			"github-releases.githubusercontent.com",
			"media.githubusercontent.com",
			"user-images.githubusercontent.com",
			"avatars.githubusercontent.com",
			"camo.githubusercontent.com",
			"github.githubassets.com",
			"collector.github.com",
		},
		Prefixes:      make(map[string]string),
		ProbePaths:    []string{"/favicon.ico", "/"},
		ProbeInterval: "5m",
		ProbeTimeout:  "3s",
		TopN:          3,
		CertDir:       "./certs",
	}
}

// LoadConfig 从 YAML 文件加载配置
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 配置文件不存在，返回默认配置
			cfg := DefaultConfig()
			fmt.Printf("[config] 配置文件 %s 不存在，使用默认配置\n", path)
			return cfg, nil
		}
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 验证配置
	if cfg.Listen == "" {
		cfg.Listen = ":9910"
	}
	if len(cfg.DoH) == 0 {
		cfg.DoH = []string{"https://dns.alidns.com/resolve"}
	}
	if len(cfg.ProbePaths) == 0 {
		cfg.ProbePaths = []string{"/"}
	}
	if cfg.TopN < 1 {
		cfg.TopN = 1
	}

	return cfg, nil
}

// ParseDuration 安全解析时间字符串
func ParseDuration(s string, defaultDur time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultDur
	}
	return d
}
