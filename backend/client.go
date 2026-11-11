package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrRisk 表示触发了 B 站风控，调用方应当停止重试并提示用户。
var ErrRisk = errors.New("触发 B 站风控，请求已暂停，请稍后再试")

type BiliError struct {
	Code    int
	Message string
}

func (e *BiliError) Error() string { return fmt.Sprintf("bilibili: %d %s", e.Code, e.Message) }

// pacer 是所有 B 站请求的唯一节流闸门：全局最小间隔 + 风控冷却。
type pacer struct {
	mu        sync.Mutex
	interval  time.Duration
	last      time.Time
	coolUntil time.Time
}

func (p *pacer) setInterval(d time.Duration) {
	p.mu.Lock()
	p.interval = d
	p.mu.Unlock()
}

func (p *pacer) cooldown(d time.Duration) {
	p.mu.Lock()
	until := time.Now().Add(d)
	if until.After(p.coolUntil) {
		p.coolUntil = until
	}
	p.mu.Unlock()
}

func (p *pacer) coolRemain() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Until(p.coolUntil)
}

func (p *pacer) wait() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Now().Before(p.coolUntil) {
		return ErrRisk
	}
	// 在最小间隔基础上加 ±25% 抖动，避免固定节奏的机器特征
	jitter := time.Duration(float64(p.interval) * (0.75 + rand.Float64()*0.5))
	if p.interval <= 0 {
		jitter = 0
	}
	if !p.last.IsZero() {
		if d := time.Until(p.last.Add(jitter)); d > 0 {
			p.mu.Unlock()
			time.Sleep(d)
			p.mu.Lock()
		}
	}
	p.last = time.Now()
	return nil
}

// mixinKeyEncTab 是 B 站 WBI 官方重排表。
var mixinKeyEncTab = []int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35,
	27, 43, 5, 49, 33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13,
	37, 48, 7, 16, 24, 55, 40, 61, 26, 17, 0, 1, 60, 51, 30, 4,
	22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11, 36, 20, 34, 44, 52,
}

type Bili struct {
	store *ConfigStore
	pace  *pacer

	clientMu sync.Mutex
	client   *http.Client
	proxy    string

	wbiMu  sync.Mutex
	wbiImg string
	wbiSub string
	wbiAt  time.Time

	buvidMu sync.Mutex

	navMu   sync.Mutex
	navAt   time.Time
	navVal  LoginState
	navErr  error
}

func newBili(store *ConfigStore) *Bili {
	c := store.Get()
	b := &Bili{store: store, pace: &pacer{}}
	b.pace.setInterval(builtinAPIMinIntervalMS * time.Millisecond)
	b.setProxy(c.Proxy)
	go b.ensureBuvid()
	return b
}

func (b *Bili) userAgent() string {
	return b.store.Get().UserAgent
}

func (b *Bili) setProxy(proxy string) {
	b.clientMu.Lock()
	defer b.clientMu.Unlock()
	if b.client != nil && b.proxy == proxy {
		return
	}
	tr := &http.Transport{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	if proxy != "" {
		if pu, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	b.proxy = proxy
	b.client = &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

func (b *Bili) httpClient() *http.Client {
	b.clientMu.Lock()
	defer b.clientMu.Unlock()
	return b.client
}

func (b *Bili) cookieHeader() string {
	c := b.store.Get()
	order := []string{"SESSDATA", "bili_jct", "DedeUserID", "DedeUserID__ckMd5", "sid"}
	var parts []string
	seen := map[string]bool{}
	for _, k := range order {
		if v := c.Cookies[k]; v != "" {
			parts = append(parts, k+"="+v)
			seen[k] = true
		}
	}
	for k, v := range c.Cookies {
		if !seen[k] && v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	if c.Buvid3 != "" {
		parts = append(parts, "buvid3="+c.Buvid3)
	}
	if c.Buvid4 != "" {
		parts = append(parts, "buvid4="+c.Buvid4)
	}
	if c.UUID != "" {
		parts = append(parts, "_uuid="+c.UUID)
	}
	return strings.Join(parts, "; ")
}

func (b *Bili) baseHeaders() map[string]string {
	return map[string]string{
		"User-Agent":      b.userAgent(),
		"Referer":         "https://www.bilibili.com/",
		"Origin":          "https://www.bilibili.com",
		"Accept":          "application/json, text/plain, */*",
		"Accept-Language": "zh-CN,zh;q=0.9",
	}
}

// ensureBuvid 只在首次请求或缺失时拉取一次，用于降低"无设备指纹"的异常特征。
func (b *Bili) ensureBuvid() {
	c := b.store.Get()
	if c.Buvid3 != "" && c.Buvid4 != "" {
		return
	}
	b.buvidMu.Lock()
	defer b.buvidMu.Unlock()
	c = b.store.Get()
	if c.Buvid3 != "" && c.Buvid4 != "" {
		return
	}
	_ = b.pace.wait()
	req, err := http.NewRequest("GET", "https://api.bilibili.com/x/frontend/finger/spi", nil)
	if err != nil {
		return
	}
	for k, v := range b.baseHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code int `json:"code"`
		Data struct {
			B3 string `json:"b_3"`
			B4 string `json:"b_4"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &env) == nil && env.Code == 0 {
		_ = b.store.Update(func(cfg *Config) {
			cfg.Buvid3 = env.Data.B3
			cfg.Buvid4 = env.Data.B4
		})
	}
}

// doRaw 发一次原始请求，不做 JSON 解包，供 passport 登录接口读取 Set-Cookie。
func (b *Bili) doRaw(rawURL string, post bool, form url.Values) ([]byte, http.Header, error) {
	if err := b.pace.wait(); err != nil {
		return nil, nil, err
	}
	var body io.Reader
	if post {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest("GET", rawURL, body)
	if err != nil {
		return nil, nil, err
	}
	if post {
		req.Method = "POST"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range b.baseHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Cookie", b.cookieHeader())
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == 412 || resp.StatusCode == 429 {
		b.pace.cooldown(5 * time.Minute)
		return nil, resp.Header, ErrRisk
	}
	return data, resp.Header, nil
}

func wbiMixinKey(imgKey, subKey string) string {
	raw := imgKey + subKey
	if len(raw) < 64 {
		return ""
	}
	var sb strings.Builder
	for _, idx := range mixinKeyEncTab {
		sb.WriteByte(raw[idx])
	}
	return sb.String()
}

func wbiEscape(s string) string {
	e := url.QueryEscape(s)
	return strings.NewReplacer("%21", "!", "%27", "'", "%28", "(", "%29", ")", "%2A", "*").Replace(e)
}

func wbiSign(params url.Values, mixin string, ts int64) url.Values {
	out := url.Values{}
	for k, vs := range params {
		out[k] = vs
	}
	out.Set("wts", strconv.FormatInt(ts, 10))
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(wbiEscape(k))
		sb.WriteByte('=')
		sb.WriteString(wbiEscape(out.Get(k)))
	}
	sb.WriteString(mixin)
	sum := md5.Sum([]byte(sb.String()))
	out.Set("w_rid", hex.EncodeToString(sum[:]))
	return out
}

// ensureWBI 拉取并缓存 nav 中的 WBI 密钥。
func (b *Bili) ensureWBI(force bool) (string, error) {
	b.wbiMu.Lock()
	defer b.wbiMu.Unlock()
	if !force && b.wbiImg != "" && time.Since(b.wbiAt) < 30*time.Minute {
		return wbiMixinKey(b.wbiImg, b.wbiSub), nil
	}
	data, err := b.requestJSON("https://api.bilibili.com/x/web-interface/nav", nil, false)
	if err != nil {
		return "", err
	}
	wi, _ := data["wbi_img"].(map[string]any)
	if wi == nil {
		return "", errors.New("nav 未返回 WBI 密钥")
	}
	imgURL, _ := wi["img_url"].(string)
	subURL, _ := wi["sub_url"].(string)
	img := fileStem(imgURL)
	sub := fileStem(subURL)
	if img == "" || sub == "" {
		return "", errors.New("无法获取 WBI 密钥")
	}
	b.wbiImg, b.wbiSub, b.wbiAt = img, sub, time.Now()
	return wbiMixinKey(img, sub), nil
}

func fileStem(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	if i := strings.Index(u, "."); i >= 0 {
		u = u[:i]
	}
	return u
}

// requestJSON 是带 WBI 签名、节流、风控识别与退避重试的统一入口。
func (b *Bili) requestJSON(rawURL string, params url.Values, signed bool) (map[string]any, error) {
	return b.requestJSONRef(rawURL, params, signed, "")
}

// requestJSONRef 是 requestJSON 的扩展版：可指定 Referer。
// 番剧播放接口要求 Referer 为 bangumi/play/ep{id} 页面。
func (b *Bili) requestJSONRef(rawURL string, params url.Values, signed bool, referer string) (map[string]any, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1500*(1<<uint(attempt-1))) * time.Millisecond
			backoff += time.Duration(rand.Intn(800)) * time.Millisecond
			time.Sleep(backoff)
		}
		if err := b.pace.wait(); err != nil {
			return nil, err
		}
		q := params
		if q == nil {
			q = url.Values{}
		} else {
			cp := url.Values{}
			for k, vs := range q {
				cp[k] = vs
			}
			q = cp
		}
		if signed {
			mixin, err := b.ensureWBI(false)
			if err != nil {
				return nil, err
			}
			q = wbiSign(q, mixin, time.Now().Unix())
		}
		full := rawURL
		if enc := q.Encode(); enc != "" {
			full += "?" + enc
		}
		req, err := http.NewRequest("GET", full, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range b.baseHeaders() {
			req.Header.Set(k, v)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		req.Header.Set("Cookie", b.cookieHeader())
		resp, err := b.httpClient().Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		if resp.StatusCode == 412 || resp.StatusCode == 429 {
			b.pace.cooldown(5 * time.Minute)
			return nil, ErrRisk
		}
		var env struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			lastErr = fmt.Errorf("响应解析失败: %w", err)
			continue
		}
		switch env.Code {
		case 0:
		case -101:
			// 未登录：nav 仍会返回 wbi_img，因此不视为失败，继续解析 data
		case -352, -412, -509, -799, -403:
			b.pace.cooldown(10 * time.Minute)
			if signed {
				_, _ = b.ensureWBI(true)
			}
			return nil, ErrRisk
		default:
			return nil, &BiliError{Code: env.Code, Message: env.Message}
		}
		// 普通接口用 data 通道；番剧/影视等 PGC 接口用 result 通道。
		payload := env.Data
		if len(payload) == 0 || string(payload) == "null" {
			payload = env.Result
		}
		if len(payload) == 0 || string(payload) == "null" {
			return map[string]any{}, nil
		}
		var data map[string]any
		if err := json.Unmarshal(payload, &data); err != nil {
			return map[string]any{"_raw": string(payload)}, nil
		}
		return data, nil
	}
	if lastErr == nil {
		lastErr = errors.New("请求失败")
	}
	return nil, lastErr
}

func (b *Bili) cooldownRemaining() int64 {
	d := b.pace.coolRemain()
	if d <= 0 {
		return 0
	}
	return int64(d.Seconds())
}

// mediaClient 用于音视频 CDN 分片下载：不设总超时，避免大分片被中断。
func (b *Bili) mediaClient() *http.Client {
	c := b.store.Get()
	tr := &http.Transport{
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 4,
		ForceAttemptHTTP2:   true,
	}
	if c.Proxy != "" {
		if pu, err := url.Parse(c.Proxy); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{Transport: tr}
}

func (b *Bili) applyConfig() {
	c := b.store.Get()
	b.pace.setInterval(builtinAPIMinIntervalMS * time.Millisecond)
	b.setProxy(c.Proxy)
}
// NavCached 返回缓存的登录状态，最多 30 秒刷新一次。
// 界面首屏与轮询都走这里，避免每个 /api/status 都发起一次网络请求
// （那会让窗口打开后长时间停留在「检测中」）。
func (b *Bili) NavCached() (LoginState, error) {
	b.navMu.Lock()
	defer b.navMu.Unlock()
	if !b.navAt.IsZero() && time.Since(b.navAt) < 30*time.Second {
		return b.navVal, b.navErr
	}
	st, err := b.Nav()
	// 失败也缓存，避免风控期间反复重试加剧问题
	b.navVal, b.navErr, b.navAt = st, err, time.Now()
	if err != nil && b.pace.coolRemain() > 0 {
		b.navAt = time.Now().Add(20 * time.Second)
	}
	return st, err
}

// InvalidateNav 在登录/退出后强制刷新。
func (b *Bili) InvalidateNav() {
	b.navMu.Lock()
	b.navAt = time.Time{}
	b.navMu.Unlock()
}

// FetchImage 以正确的 Referer 拉取 B 站图片（封面、头像）。
// 浏览器直接引用 i0.hdslb.com 会被防盗链拦截（403），必须服务端代取。
func (b *Bili) FetchImage(rawURL string) ([]byte, string, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", b.userAgent())
	req.Header.Set("Referer", "https://www.bilibili.com/")
	req.Header.Set("Accept", "image/avif,image/webp,image/*,*/*;q=0.8")
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("图片请求失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	return data, ct, nil
}
