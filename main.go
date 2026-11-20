package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"
)

type config struct {
	APIKey       string         `json:"api_key"`
	IntervalSecs int            `json:"interval_seconds"`
	Records      []recordConfig `json:"records"`
}

type recordConfig struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	CheckURL  string `json:"check_url"`
	UpdateURL string `json:"update_url"`
	TTL       int    `json:"ttl"`
	Proxied   bool   `json:"proxied"`
}

func loadConfig(path string, allowEnv bool) (config, error) {
	data, err := os.ReadFile(path)
	var cfg config
	if os.IsNotExist(err) && allowEnv && os.Getenv("CF_API_KEY") != "" {
		cfg.APIKey = os.Getenv("CF_API_KEY")
		cfg.Records = []recordConfig{{Type: "AAAA", Name: os.Getenv("IPV6_LOCAL_URL"), CheckURL: os.Getenv("IPV6_CHECK_URL"), UpdateURL: os.Getenv("DDNS_UPDATE_URL")}}
	} else if err != nil {
		return config{}, fmt.Errorf("读取配置文件 %q 失败: %w", path, err)
	} else if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	if cfg.APIKey == "" {
		return config{}, fmt.Errorf("配置缺少 api_key")
	}
	if cfg.IntervalSecs <= 0 {
		cfg.IntervalSecs = 60
	}
	if len(cfg.Records) == 0 {
		return config{}, fmt.Errorf("配置 records 不能为空")
	}
	for i := range cfg.Records {
		r := &cfg.Records[i]
		r.Type = strings.ToUpper(strings.TrimSpace(r.Type))
		if r.Type != "A" && r.Type != "AAAA" {
			return config{}, fmt.Errorf("records[%d].type 必须是 A 或 AAAA", i)
		}
		if strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.CheckURL) == "" || strings.TrimSpace(r.UpdateURL) == "" {
			return config{}, fmt.Errorf("records[%d] 必须配置 name、check_url 和 update_url", i)
		}
		if r.TTL <= 0 {
			r.TTL = 3600
		}
	}
	return cfg, nil
}

func probeClient(network string) *http.Client {
	dialer := &net.Dialer{}
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
	}}
}

func getIP(client *http.Client, endpoint, recordType string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("IP 检测接口返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil || (recordType == "A" && !ip.Is4()) || (recordType == "AAAA" && (!ip.Is6() || ip.Is4In6())) {
		return "", fmt.Errorf("检测接口未返回有效的 %s 地址", recordType)
	}
	return ip.String(), nil
}

func ddnsUpdate(client *http.Client, apiKey string, record recordConfig, ip string) error {
	payload := map[string]any{
		"content": ip,
		"name":    record.Name,
		"type":    record.Type,
		"ttl":     record.TTL,
		"proxied": record.Proxied,
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPatch, record.UpdateURL, bytes.NewReader(jsonPayload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Cloudflare API 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("无法解析 Cloudflare 响应: %w", err)
	}
	if !result.Success {
		messages := make([]string, 0, len(result.Errors))
		for _, item := range result.Errors {
			messages = append(messages, item.Message)
		}
		return fmt.Errorf("Cloudflare API 更新失败: %s", strings.Join(messages, "; "))
	}
	fmt.Printf("[%s] %s 记录 %s 已更新为: %s\n", time.Now().Format("15:04:05"), record.Type, record.Name, ip)
	return nil
}

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()
	cfg, err := loadConfig(*configPath, flag.NFlag() == 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	client := &http.Client{Timeout: 20 * time.Second}
	probes := map[string]*http.Client{"A": probeClient("tcp4"), "AAAA": probeClient("tcp6")}
	oldIPs := make([]string, len(cfg.Records))
	for {
		for i, record := range cfg.Records {
			ip, err := getIP(probes[record.Type], record.CheckURL, record.Type)
			if err != nil {
				fmt.Printf("%s %s 检测失败: %v\n", record.Type, record.Name, err)
				continue
			}
			if ip == oldIPs[i] {
				continue
			}
			if err := ddnsUpdate(client, cfg.APIKey, record, ip); err != nil {
				fmt.Printf("%s %s 更新失败: %v\n", record.Type, record.Name, err)
				continue
			}
			oldIPs[i] = ip
		}
		time.Sleep(time.Duration(cfg.IntervalSecs) * time.Second)
	}
}
