package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// 内置合理的 API 最小请求间隔（毫秒），配合随机抖动降低风控概率。
const builtinAPIMinIntervalMS = 700

// Config 是持久化到 var/config.json 的全部配置项。
type Config struct {
	DownloadDir      string            `json:"download_dir"`
	Port             int               `json:"port"`
	MaxTasks         int               `json:"max_tasks"`
	ChunkConcurrency int               `json:"chunk_concurrency"`
	BandwidthKB      int               `json:"bandwidth_kb"`
	Proxy            string            `json:"proxy"`
	UserAgent        string            `json:"user_agent"`
	FilenameTemplate string            `json:"filename_template"`
	QN               int               `json:"qn"`
	VideoCodec       int               `json:"video_codec"`
	AudioID          int               `json:"audio_id"`
	Cookies          map[string]string `json:"cookies"`
	Buvid3           string            `json:"buvid3"`
	Buvid4           string            `json:"buvid4"`
	UUID             string            `json:"uuid"`
}

func defaultConfig() Config {
	return Config{
		Port:             18088,
		MaxTasks:         2,
		ChunkConcurrency: 4,
		UserAgent:        defaultUserAgent,
		FilenameTemplate: "{title} [{quality}]",
		QN:               80,
		VideoCodec:       0,
		AudioID:          30280,
		Cookies:          map[string]string{},
	}
}

// ConfigStore 提供并发安全的配置读写与落盘。
type ConfigStore struct {
	mu  sync.RWMutex
	cfg Config
	dir string
}

func newConfigStore(dir string) (*ConfigStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &ConfigStore{dir: dir, cfg: defaultConfig()}
	path := filepath.Join(dir, "config.json")
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &s.cfg)
	}
	if s.cfg.Cookies == nil {
		s.cfg.Cookies = map[string]string{}
	}
	if s.cfg.MaxTasks < 1 {
		s.cfg.MaxTasks = 1
	}
	if s.cfg.MaxTasks > 10 {
		s.cfg.MaxTasks = 10
	}
	if s.cfg.ChunkConcurrency < 1 {
		s.cfg.ChunkConcurrency = 1
	}
	if s.cfg.ChunkConcurrency > 32 {
		s.cfg.ChunkConcurrency = 32
	}
	if s.cfg.Port <= 0 {
		s.cfg.Port = 18088
	}
	if strings.TrimSpace(s.cfg.UserAgent) == "" {
		s.cfg.UserAgent = defaultUserAgent
	}
	if s.cfg.UUID == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		s.cfg.UUID = hex.EncodeToString(b[:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
			hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:])
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *ConfigStore) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	if strings.TrimSpace(c.UserAgent) == "" {
		c.UserAgent = defaultUserAgent
	}
	c.Cookies = map[string]string{}
	for k, v := range s.cfg.Cookies {
		c.Cookies[k] = v
	}
	return c
}

func (s *ConfigStore) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	return s.save()
}

func (s *ConfigStore) save() error {
	raw, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "config.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *ConfigStore) Dir() string { return s.dir }

func (s *ConfigStore) SetCookies(cookies map[string]string) error {
	return s.Update(func(c *Config) {
		c.Cookies = cookies
	})
}

// probeWritable 通过写入临时探针文件真实验证目录可写权限。
func probeWritable(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("目录为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("无法创建目录：%w", err)
	}
	probe := filepath.Join(dir, ".bilidock-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("目录不可写：%w", err)
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return nil
}

// ShareCandidate 表示一个可用的下载目录候选位置。
type ShareCandidate struct {
	Path   string `json:"path"`
	Label  string `json:"label"`
	FreeGB int64  `json:"free_gb"`
}

// listWritableCandidates 仅返回套件有写权限的共享文件夹。
func listWritableCandidates() []ShareCandidate {
	out := make([]ShareCandidate, 0, 8)
	skip := map[string]bool{
		"@appstore": true, "@database": true, "@eaDir": true, "@tmp": true,
		"@synocache": true, "@synologydrive": true, "@docker": true,
		"@iSCSI": true, "@S2S": true, "@spool": true, "@surveillance": true,
	}
	for i := 1; i <= 8; i++ {
		vol := fmt.Sprintf("/volume%d", i)
		st, err := os.Stat(vol)
		if err != nil || !st.IsDir() {
			continue
		}
		entries, err := os.ReadDir(vol)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), "@") || skip[e.Name()] {
				continue
			}
			full := filepath.Join(vol, e.Name())
			if probeWritable(full) != nil {
				continue
			}
			c := ShareCandidate{Path: full, Label: full}
			if free, ok := freeGB(full); ok {
				c.FreeGB = free
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		return out[a].Path < out[b].Path
	})
	return out
}
