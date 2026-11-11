package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Task struct {
	ID           string `json:"id"`
	Bvid         string `json:"bvid"`
	Aid          int64  `json:"aid"`
	Cid          int64  `json:"cid"`
	EpID         int64  `json:"ep_id"`
	SeasonID     int64  `json:"season_id"`
	IsPgc        bool   `json:"is_pgc"`
	Title        string `json:"title"`
	Cover        string `json:"cover"`
	PageTitle    string `json:"page_title"`
	QN           int    `json:"qn"`
	VideoCodec   int    `json:"video_codec"`
	AudioID      int    `json:"audio_id"`
	QualityLabel string `json:"quality_label"`
	CodecLabel   string `json:"codec_label"`
	AudioLabel   string `json:"audio_label"`
	Status       string `json:"status"`
	Stage        string `json:"stage"`
	Downloaded   int64  `json:"downloaded"`
	Total        int64  `json:"total"`
	Speed        int64  `json:"speed"`
	Error        string `json:"error"`
	Warning      string `json:"warning"`
	OutputPath   string `json:"output_path"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`

	downloaded int64
	lastBytes  int64
}

type Manager struct {
	mu       sync.Mutex
	tasks    []*Task
	store    *ConfigStore
	bili     *Bili
	varDir   string
	ffmpeg   string
	queue    chan string
	cancels  map[string]context.CancelFunc
	limiter  *rateLimiter
	dispMu   sync.Mutex
	dispCond *sync.Cond
	active   int
}

type rateLimiter struct {
	mu    sync.Mutex
	rate  int64
	next  time.Time
}

func (r *rateLimiter) wait(n int64) {
	r.mu.Lock()
	rate := r.rate
	if rate <= 0 || n <= 0 {
		r.mu.Unlock()
		return
	}
	now := time.Now()
	if r.next.Before(now) {
		r.next = now
	}
	r.next = r.next.Add(time.Duration(float64(n) / float64(rate) * float64(time.Second)))
	wait := r.next.Sub(now)
	r.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
}

func newManager(store *ConfigStore, bili *Bili, varDir, ffmpeg string) *Manager {
	cfg := store.Get()
	m := &Manager{
		store:   store,
		bili:    bili,
		varDir:  varDir,
		ffmpeg:  ffmpeg,
		queue:   make(chan string, 256),
		cancels: map[string]context.CancelFunc{},
		limiter: &rateLimiter{rate: int64(cfg.BandwidthKB) * 1024},
	}
	m.dispCond = sync.NewCond(&m.dispMu)
	m.load()
	go m.dispatcher()
	return m
}

func (m *Manager) tasksPath() string { return filepath.Join(m.varDir, "tasks.json") }

type persistedTask struct {
	Tasks []*Task `json:"tasks"`
}

func (m *Manager) load() {
	raw, err := os.ReadFile(m.tasksPath())
	if err != nil {
		return
	}
	var p persistedTask
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	now := time.Now().Unix()
	for _, t := range p.Tasks {
		if t.Status == "downloading" || t.Status == "muxing" || t.Status == "queued" {
			t.Status = "paused"
			t.Stage = "已暂停（服务重启）"
		}
		t.UpdatedAt = now
		m.tasks = append(m.tasks, t)
	}
}

func (m *Manager) persistLocked() {
	p := persistedTask{Tasks: m.tasks}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	tmp := m.tasksPath() + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, m.tasksPath())
	}
}

func (m *Manager) Snapshot() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		cp := *t
		out = append(out, cp)
	}
	return out
}

func (m *Manager) get(id string) *Task {
	for _, t := range m.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// CreatePgc 新建番剧/影视下载任务；同一集同规格若未结束则复用。
func (m *Manager) CreatePgc(info PgcInfo, ep PgcEpisode, qn, codecID, audioID int) (*Task, error) {
	cfg := m.store.Get()
	if qn <= 0 {
		qn = cfg.QN
	}
	if codecID < 0 {
		codecID = cfg.VideoCodec
	}
	if audioID <= 0 {
		audioID = cfg.AudioID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.EpID == ep.EpID && t.QN == qn && t.VideoCodec == codecID && t.AudioID == audioID &&
			t.Status != "done" && t.Status != "failed" {
			return t, nil
		}
	}
	title := info.Title
	if ep.LongTitle != "" {
		title = info.Title + " - " + ep.LongTitle
	} else if ep.Title != "" {
		title = info.Title + " - " + ep.Title
	}
	task := &Task{
		ID:           randomID(),
		Bvid:         ep.Bvid,
		Aid:          ep.Aid,
		Cid:          ep.Cid,
		EpID:         ep.EpID,
		SeasonID:     info.SeasonID,
		IsPgc:        true,
		Title:        title,
		Cover:        ep.Cover,
		PageTitle:    firstNonEmpty(ep.LongTitle, ep.Title),
		QN:           qn,
		VideoCodec:   codecID,
		AudioID:      audioID,
		QualityLabel: QualityLabel(qn),
		CodecLabel:   CodecLabel(codecID),
		AudioLabel:   AudioLabel(audioID),
		Status:       "queued",
		Stage:        "排队中",
		CreatedAt:    time.Now().Unix(),
		UpdatedAt:    time.Now().Unix(),
	}
	if task.Cover == "" {
		task.Cover = info.Cover
	}
	m.tasks = append([]*Task{task}, m.tasks...)
	m.persistLocked()
	go func() { m.queue <- task.ID }()
	return task, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (m *Manager) Create(info VideoInfo, page int, qn, codecID, audioID int) (*Task, error) {
	cfg := m.store.Get()
	cid := info.Cid
	pageTitle := ""
	if len(info.Pages) > 0 {
		idx := page - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(info.Pages) {
			idx = len(info.Pages) - 1
		}
		cid = info.Pages[idx].Cid
		pageTitle = info.Pages[idx].Part
	}
	if cid == 0 {
		return nil, fmt.Errorf("未获取到 cid")
	}
	if qn <= 0 {
		qn = cfg.QN
	}
	if codecID < 0 {
		codecID = cfg.VideoCodec
	}
	if audioID <= 0 {
		audioID = cfg.AudioID
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.Cid == cid && t.QN == qn && t.VideoCodec == codecID && t.AudioID == audioID &&
			t.Status != "done" && t.Status != "failed" {
			return t, nil
		}
	}
	id := randomID()
	task := &Task{
		ID:           id,
		Bvid:         info.Bvid,
		Aid:          info.Aid,
		Cid:          cid,
		Title:        info.Title,
		Cover:        info.Cover,
		PageTitle:    pageTitle,
		QN:           qn,
		VideoCodec:   codecID,
		AudioID:      audioID,
		QualityLabel: QualityLabel(qn),
		CodecLabel:   CodecLabel(codecID),
		AudioLabel:   AudioLabel(audioID),
		Status:       "queued",
		Stage:        "排队中",
		CreatedAt:    time.Now().Unix(),
		UpdatedAt:    time.Now().Unix(),
	}
	m.tasks = append([]*Task{task}, m.tasks...)
	m.persistLocked()
	go func() { m.queue <- id }()
	return task, nil
}

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) dispatcher() {
	for id := range m.queue {
		m.dispMu.Lock()
		for {
			limit := m.store.Get().MaxTasks
			if limit < 1 {
				limit = 1
			}
			if m.active < limit {
				break
			}
			m.dispCond.Wait()
		}
		m.active++
		m.dispMu.Unlock()
		go func(id string) {
			defer func() {
				m.dispMu.Lock()
				m.active--
				m.dispCond.Broadcast()
				m.dispMu.Unlock()
			}()
			m.run(id)
		}(id)
	}
}

func (m *Manager) setStatus(id, status, stage string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.get(id); t != nil {
		t.Status = status
		if stage != "" {
			t.Stage = stage
		}
		t.UpdatedAt = time.Now().Unix()
		m.persistLocked()
	}
}

func (m *Manager) fail(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.get(id); t != nil {
		t.Status = "failed"
		t.Error = err.Error()
		t.Stage = "失败"
		t.UpdatedAt = time.Now().Unix()
		m.persistLocked()
	}
}

func (m *Manager) ClearDone() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.tasks[:0]
	removed := 0
	for _, x := range m.tasks {
		if x.Status == "done" {
			removed++
			continue
		}
		kept = append(kept, x)
	}
	m.tasks = kept
	m.persistLocked()
	return removed
}

func (m *Manager) Action(id, action string) error {
	if action == "clear_done" {
		m.ClearDone()
		return nil
	}
	m.mu.Lock()
	t := m.get(id)
	if t == nil {
		m.mu.Unlock()
		return fmt.Errorf("任务不存在")
	}
	switch action {
	case "pause", "cancel":
		if c := m.cancels[id]; c != nil {
			c()
		}
		t.Status = "paused"
		t.Stage = "已暂停"
		t.UpdatedAt = time.Now().Unix()
		m.persistLocked()
		m.mu.Unlock()
		return nil
	case "resume", "retry":
		if t.Status == "done" {
			m.mu.Unlock()
			return fmt.Errorf("任务已完成")
		}
		t.Status = "queued"
		t.Stage = "排队中"
		t.Error = ""
		t.UpdatedAt = time.Now().Unix()
		m.persistLocked()
		m.mu.Unlock()
		go func() { m.queue <- id }()
		return nil
	case "remove":
		// 仅移除队列记录，保留已下载到 NAS 的音视频文件
		if c := m.cancels[id]; c != nil {
			c()
		}
		filtered := m.tasks[:0]
		for _, x := range m.tasks {
			if x.ID != id {
				filtered = append(filtered, x)
			}
		}
		m.tasks = filtered
		m.persistLocked()
		m.mu.Unlock()
		_ = os.Remove(filepath.Join(m.varDir, "tmp", id+".video"))
		_ = os.Remove(filepath.Join(m.varDir, "tmp", id+".audio"))
		return nil
	case "delete":
		if c := m.cancels[id]; c != nil {
			c()
		}
		out := t.OutputPath
		filtered := m.tasks[:0]
		for _, x := range m.tasks {
			if x.ID != id {
				filtered = append(filtered, x)
			}
		}
		m.tasks = filtered
		m.persistLocked()
		m.mu.Unlock()
		if out != "" {
			_ = os.Remove(out)
		}
		_ = os.Remove(filepath.Join(m.varDir, "tmp", id+".video"))
		_ = os.Remove(filepath.Join(m.varDir, "tmp", id+".audio"))
		return nil
	}
	m.mu.Unlock()
	return fmt.Errorf("未知操作")
}

// StartSpeedTicker 每秒刷新一次速度字段，供界面与 SSE 使用。
func (m *Manager) StartSpeedTicker() {
	go func() {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for range tk.C {
			m.mu.Lock()
			for _, t := range m.tasks {
				if t.Status == "downloading" {
					t.Speed = t.Downloaded - t.lastBytes
				} else {
					t.Speed = 0
				}
				t.lastBytes = t.Downloaded
			}
			m.mu.Unlock()
		}
	}()
}

// run 执行一个下载任务：解析地址 → 分片下载 → ffmpeg 混流 → 落盘。
func (m *Manager) run(id string) {
	m.mu.Lock()
	t := m.get(id)
	m.mu.Unlock()
	if t == nil {
		return
	}
	cfg := m.store.Get()
	m.applyRuntimeConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancels[id] = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.cancels, id)
		m.mu.Unlock()
	}()

	m.setStatus(id, "downloading", "解析播放地址")
	var (
		res PlayResult
		err error
	)
	if t.IsPgc && t.EpID > 0 {
		res, err = m.bili.PgcPlayURL(PgcEpisode{EpID: t.EpID, Aid: t.Aid, Bvid: t.Bvid, Cid: t.Cid}, t.QN)
	} else {
		res, err = m.bili.PlayURL(t.Bvid, t.Aid, t.Cid, t.QN)
	}
	if err != nil {
		m.fail(id, err)
		return
	}
	video, audio := PickStreams(res, t.QN, t.VideoCodec, t.AudioID)
	if video == nil && audio == nil {
		m.fail(id, fmt.Errorf("没有符合条件的流（可能需要登录或大会员）"))
		return
	}

	tmpDir := filepath.Join(m.varDir, "tmp")
	_ = os.MkdirAll(tmpDir, 0o700)
	videoPath := filepath.Join(tmpDir, id+".video")
	audioPath := filepath.Join(tmpDir, id+".audio")

	client := m.bili.mediaClient()
	ua := m.bili.userAgent()
	var total int64
	var videoSize, audioSize int64
	if video != nil {
		sz, _, err := probeSize(ctx, client, ua, append([]string{video.URL}, video.Backup...))
		if err != nil {
			m.fail(id, fmt.Errorf("探测视频大小失败: %w", err))
			return
		}
		videoSize = sz
		total += sz
	}
	if audio != nil {
		sz, _, err := probeSize(ctx, client, ua, append([]string{audio.URL}, audio.Backup...))
		if err != nil {
			m.fail(id, fmt.Errorf("探测音频大小失败: %w", err))
			return
		}
		audioSize = sz
		total += sz
	}
	m.mu.Lock()
	if x := m.get(id); x != nil {
		x.Total = total
	}
	m.mu.Unlock()

	m.setStatus(id, "downloading", "下载中")
	var done int64
	report := func(n int64) {
		atomic.AddInt64(&done, n)
		m.mu.Lock()
		if x := m.get(id); x != nil {
			x.Downloaded = atomic.LoadInt64(&done)
		}
		m.mu.Unlock()
	}
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	setErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
		}
		errMu.Unlock()
	}
	if video != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := downloadStream(ctx, client, ua, m.limiter, video, videoPath, videoSize, cfg.ChunkConcurrency, report); err != nil {
				setErr(err)
			}
		}()
	}
	if audio != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := downloadStream(ctx, client, ua, m.limiter, audio, audioPath, audioSize, 1, report); err != nil {
				setErr(err)
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		if ctx.Err() != nil {
			m.setStatus(id, "paused", "已暂停")
			return
		}
		m.fail(id, firstErr)
		return
	}

	m.setStatus(id, "muxing", "混流中")

	// 目标目录不可写时，回落到套件私有目录，任务降级为「已完成（默认位置）」，
	// 避免整段下载白费。同时在任务里记录原因供界面提示。
	outDir := cfg.DownloadDir
	fallback := ""
	if err := probeWritable(outDir); err != nil {
		fb := filepath.Join(m.varDir, "downloads")
		if err2 := probeWritable(fb); err2 == nil {
			fallback = fmt.Sprintf("下载目录 %s 不可写（%v），已保存到 %s", outDir, err, fb)
			outDir = fb
		} else {
			m.fail(id, fmt.Errorf("下载目录不可写：%v；备用目录也不可用：%v", err, err2))
			return
		}
	}
	outName := buildFilename(cfg.FilenameTemplate, t)
	outPath := filepath.Join(outDir, outName)
	if err := m.mux(ctx, videoPath, audioPath, outPath); err != nil {
		m.fail(id, fmt.Errorf("混流失败: %w", err))
		return
	}
	_ = os.Remove(videoPath)
	_ = os.Remove(audioPath)

	m.mu.Lock()
	if x := m.get(id); x != nil {
		x.Status = "done"
		x.Stage = "已完成"
		x.OutputPath = outPath
		x.Downloaded = x.Total
		x.UpdatedAt = time.Now().Unix()
		if fallback != "" {
			x.Warning = fallback
		}
	}
	m.persistLocked()
	m.mu.Unlock()
}

func (m *Manager) applyRuntimeConfig(cfg Config) {
	m.bili.applyConfig()
	m.limiter.mu.Lock()
	m.limiter.rate = int64(cfg.BandwidthKB) * 1024
	m.limiter.mu.Unlock()
	if m.dispCond != nil {
		m.dispCond.Broadcast()
	}
}

func buildFilename(tpl string, t *Task) string {
	title := t.Title
	if t.PageTitle != "" && t.PageTitle != t.Title {
		title = t.PageTitle
	}
	out := strings.NewReplacer(
		"{title}", title,
		"{quality}", t.QualityLabel,
		"{audio}", t.AudioLabel,
		"{codec}", t.CodecLabel,
		"{bvid}", t.Bvid,
		"{date}", time.Now().Format("20060102"),
	).Replace(tpl)
	if strings.TrimSpace(out) == "" {
		out = title
	}
	return sanitize(out) + ".mp4"
}

var reIllegal = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)

func sanitize(s string) string {
	s = reIllegal.ReplaceAllString(s, "_")
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120])
	}
	if s == "" {
		s = "video"
	}
	return s
}

// probeSize 用 Range 0-0 探测文件总大小。
func probeSize(ctx context.Context, client *http.Client, ua string, urls []string) (int64, string, error) {
	var lastErr error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			continue
		}
		mediaHeaders(req, ua)
		req.Header.Set("Range", "bytes=0-0")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		cr := resp.Header.Get("Content-Range")
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if resp.StatusCode == 206 && cr != "" {
			if i := strings.LastIndex(cr, "/"); i >= 0 {
				if n, err := parseInt64(cr[i+1:]); err == nil && n > 0 {
					return n, u, nil
				}
			}
		}
		lastErr = fmt.Errorf("探测失败: HTTP %d", resp.StatusCode)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的下载地址")
	}
	return 0, "", lastErr
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscan(strings.TrimSpace(s), &n)
	return n, err
}

func mediaHeaders(req *http.Request, ua string) {
	if strings.TrimSpace(ua) == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", "https://www.bilibili.com/")
	req.Header.Set("Origin", "https://www.bilibili.com")
	req.Header.Set("Accept", "*/*")
}

// downloadStream 按分片并发下载，失败退避重试并轮换备用地址。
func downloadStream(ctx context.Context, client *http.Client, ua string, limiter *rateLimiter, s *StreamInfo, dst string, size int64, conc int, report func(int64)) error {
	if conc < 1 {
		conc = 1
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return err
	}

	const chunk = 4 << 20
	urls := append([]string{s.URL}, s.Backup...)
	var (
		next    int64
		mu      sync.Mutex
		firstEr error
	)
	work := func() error {
		for {
			mu.Lock()
			if firstEr != nil {
				mu.Unlock()
				return firstEr
			}
			start := next
			if start >= size {
				mu.Unlock()
				return nil
			}
			end := start + chunk - 1
			if end >= size {
				end = size - 1
			}
			next = end + 1
			mu.Unlock()

			n, err := fetchRange(ctx, client, ua, limiter, urls, start, end)
			if err != nil {
				mu.Lock()
				if firstEr == nil {
					firstEr = err
				}
				mu.Unlock()
				return err
			}
			if _, err := f.WriteAt(n, start); err != nil {
				mu.Lock()
				if firstEr == nil {
					firstEr = err
				}
				mu.Unlock()
				return err
			}
			report(int64(len(n)))
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = work()
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	return firstEr
}

func fetchRange(ctx context.Context, client *http.Client, ua string, limiter *rateLimiter, urls []string, start, end int64) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(600*(1<<uint(attempt-1))) * time.Millisecond
			backoff += time.Duration(randomInt(500)) * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		u := urls[attempt%len(urls)]
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		mediaHeaders(req, ua)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, (end-start+1)+1024))
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		if resp.StatusCode != 206 && resp.StatusCode != 200 {
			lastErr = fmt.Errorf("分片请求失败: HTTP %d", resp.StatusCode)
			if resp.StatusCode == 403 || resp.StatusCode == 412 {
				time.Sleep(time.Duration(800+randomInt(700)) * time.Millisecond)
			}
			continue
		}
		want := int(end - start + 1)
		if len(body) < want {
			lastErr = fmt.Errorf("分片长度不足: %d/%d", len(body), want)
			continue
		}
		limiter.wait(int64(want))
		return body[:want], nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("分片下载失败")
	}
	return nil, lastErr
}

func randomInt(n int) int {
	if n <= 0 {
		return 0
	}
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return (int(b[0])<<8 | int(b[1])) % n
}

// mux 调用 ffmpeg 做无损 stream copy 封装（统一输出 MP4）。
func (m *Manager) mux(ctx context.Context, videoPath, audioPath, outPath string) error {
	hasVideo := videoPath != "" && fileExists(videoPath)
	hasAudio := audioPath != "" && fileExists(audioPath)
	args := []string{"-hide_banner", "-nostdin", "-y"}
	switch {
	case hasVideo && hasAudio:
		args = append(args, "-i", audioPath, "-i", videoPath,
			"-map", "1:v:0", "-map", "0:a:0", "-c:v", "copy", "-c:a", "copy",
			"-movflags", "+faststart")
	case hasVideo:
		args = append(args, "-i", videoPath, "-map", "0:v:0", "-c:v", "copy", "-movflags", "+faststart")
	case hasAudio:
		args = append(args, "-i", audioPath, "-vn", "-c:a", "copy")
	default:
		return fmt.Errorf("没有可封装的轨道")
	}
	args = append(args, outPath)
	cmd := exec.CommandContext(ctx, m.ffmpeg, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if len(msg) > 800 {
			msg = msg[len(msg)-800:]
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(msg))
	}
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}