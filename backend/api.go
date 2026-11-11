package main

import (
	"bytes"
	"errors"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"image/png"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed web/*
var webFS embed.FS

type Server struct {
	store  *ConfigStore
	bili   *Bili
	mgr    *Manager
	varDir string
}

func NewServer(store *ConfigStore, bili *Bili, mgr *Manager, varDir string) *Server {
	return &Server{store: store, bili: bili, mgr: mgr, varDir: varDir}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/login/qr", s.handleQR)
	mux.HandleFunc("/api/login/poll", s.handlePoll)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/parse", s.handleParse)
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/tasks/action", s.handleTaskAction)
	mux.HandleFunc("/api/tasks/file", s.handleTaskFile)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/files", s.handleFiles)
	mux.HandleFunc("/api/cover", s.handleCover)
	mux.HandleFunc("/api/dirs", s.handleDirs)
	mux.HandleFunc("/api/dir/test", s.handleDirTest)
	mux.HandleFunc("/api/ep/streams", s.handleEpStreams)
	sub, _ := fs.Sub(webFS, "web")
	fileServer := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			s.serveInlineApp(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})

	// DSM 对 /xxx.cgi/<子路径> 的路由并不可靠，因此支持用查询参数携带真实路径：
	//   index.cgi?__api=/api/status
	// 这样界面只需要访问 CGI 自身，不依赖任何子资源。
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ap := r.URL.Query().Get("__api"); ap != "" {
			// __api 自身可能带查询串，例如 /api/cover?url=xxx。
			// 必须拆成 path 与 rawquery 两部分，否则整段都会被当成路径导致 404。
			u := *r.URL
			if i := strings.Index(ap, "?"); i >= 0 {
				u.Path = ap[:i]
				u.RawQuery = ap[i+1:]
			} else {
				u.Path = ap
				u.RawQuery = ""
			}
			if u.Path == "" {
				u.Path = "/"
			}
			r2 := r.Clone(r.Context())
			r2.URL = &u
			mux.ServeHTTP(w, r2)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// serveInlineApp 把 CSS 与 JS 内联进 HTML，避免任何额外的静态资源请求。
// 这样即使 DSM 只把 index.cgi 精确路径交给 CGI，界面也能完整工作。
func (s *Server) serveInlineApp(w http.ResponseWriter, r *http.Request) {
	page, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "页面资源缺失", 500)
		return
	}
	css, _ := webFS.ReadFile("web/style.css")
	js, _ := webFS.ReadFile("web/app.js")

	html := string(page)
	if logo, lerr := webFS.ReadFile("web/logo.png"); lerr == nil && len(logo) > 0 {
		logoURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(logo)
		html = strings.ReplaceAll(html, "__LOGO_DATA_URI__", logoURI)
	}
	html = strings.Replace(html,
		`<link rel="stylesheet" href="style.css">`,
		"<style>\n"+string(css)+"\n</style>", 1)
	html = strings.Replace(html,
		`<script src="app.js"></script>`,
		"<script>\n"+string(js)+"\n</script>", 1)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, html)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	// 容忍部分客户端（如 PowerShell / 旧工具）带 BOM 的请求体
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	return json.Unmarshal(raw, v)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	login, err := s.bili.NavCached()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	tasks := s.mgr.Snapshot()
	running := 0
	for _, t := range tasks {
		if t.Status == "downloading" || t.Status == "muxing" || t.Status == "queued" {
			running++
		}
	}
	writeJSON(w, 200, map[string]any{
		"version":               Version,
		"login":                 login,
		"login_error":           msg,
		"cooldown":              s.bili.cooldownRemaining(),
		"download_dir":          cfg.DownloadDir,
		"download_dir_writable": probeWritable(cfg.DownloadDir) == nil,
		"task_count":            len(tasks),
		"running_count":         running,
		"chunk_conc":            cfg.ChunkConcurrency,
		"max_tasks":             cfg.MaxTasks,
		"default_qn":            cfg.QN,
		"default_codec":         cfg.VideoCodec,
		"default_audio":         cfg.AudioID,
		"bandwidth_kb":          cfg.BandwidthKB,
		"user_agent":            cfg.UserAgent,
	})
}

type qrResp struct {
	URL string `json:"url"`
	Key string `json:"key"`
	PNG string `json:"png"`
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	info, err := s.bili.QRGenerate()
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	q, err := qrcode.New(info.URL, qrcode.Medium)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	q.DisableBorder = true
	var buf bytes.Buffer
	if err := png.Encode(&buf, q.Image(260)); err != nil {
		writeErr(w, 500, err)
		return
	}
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	writeJSON(w, 200, qrResp{URL: info.URL, Key: info.Key, PNG: dataURI})
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErr(w, 400, fmt.Errorf("缺少 key"))
		return
	}
	code, msg, err := s.bili.QRPoll(key)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	resp := map[string]any{"code": code, "message": msg}
	if code == 0 {
		s.bili.InvalidateNav()
		if st, nerr := s.bili.NavCached(); nerr == nil {
			resp["login"] = st
		}
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.store.SetCookies(map[string]string{}); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.bili.InvalidateNav()
	writeJSON(w, 200, map[string]any{"ok": true})
}

type parseReq struct {
	Input string `json:"input"`
}

type streamView struct {
	ID         int    `json:"id"`
	Label      string `json:"label"`
	CodecLabel string `json:"codec_label,omitempty"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Codecs     string `json:"codecs"`
	Codecid    int    `json:"codecid"`
	Bandwidth  int64  `json:"bandwidth"`
}

func (s *Server) handleParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("只支持 POST"))
		return
	}
	var req parseReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	cfg := s.store.Get()
	input := s.bili.ExpandShortURL(req.Input)

	// 番剧/影视/课程：走 PGC 通道
	if pin, ok := ParsePgcInput(input); ok {
		info, err := s.bili.PgcView(pin)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		// 默认选中：输入里指定了 ep 就用它，否则第一集
		defaultEp := info.Episodes[0].EpID
		for _, ep := range info.Episodes {
			if pin.EpID > 0 && ep.EpID == pin.EpID {
				defaultEp = ep.EpID
				break
			}
		}
		var (
			play    PlayResult
			playErr error
		)
		for _, ep := range info.Episodes {
			if ep.EpID == defaultEp {
				play, playErr = s.bili.PgcPlayURL(ep, 127)
				break
			}
		}
		vids, auds := summarizeStreams(play)
		notice := ""
		if playErr != nil {
			notice = playErr.Error()
		}
		writeJSON(w, 200, map[string]any{
			"kind":           "pgc",
			"resolved_input": input,
			"pgc":            info,
			"episodes":       info.Episodes,
			"default_ep":     defaultEp,
			"stream_notice":  notice,
			"videos":         vids,
			"audios":         auds,
			"defaults":       map[string]int{"qn": cfg.QN, "video_codec": cfg.VideoCodec, "audio_id": cfg.AudioID},
		})
		return
	}

	bvid, aid, page := ParseInput(input)
	info, err := s.bili.View(bvid, aid)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	play, err := s.bili.PlayURL(info.Bvid, info.Aid, info.Cid, 127)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	vids, auds := summarizeStreams(play)

	writeJSON(w, 200, map[string]any{
		"kind":           "video",
		"resolved_input": input,
		"video":          info,
		"page":           page,
		"videos":         vids,
		"audios":         auds,
		"defaults":       map[string]int{"qn": cfg.QN, "video_codec": cfg.VideoCodec, "audio_id": cfg.AudioID},
	})
}

type createTaskReq struct {
	Input      string  `json:"input"`
	Page       int     `json:"page"`
	Pages      []int   `json:"pages"`
	EpID       int64   `json:"ep_id"`
	EpIDs      []int64 `json:"ep_ids"`
	QN         int     `json:"qn"`
	VideoCodec *int    `json:"video_codec"`
	AudioID    int     `json:"audio_id"`
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"tasks": s.mgr.Snapshot()})
	case http.MethodPost:
		var req createTaskReq
		if err := readJSON(r, &req); err != nil {
			writeErr(w, 400, err)
			return
		}
		codecID := -1
		if req.VideoCodec != nil {
			codecID = *req.VideoCodec
		}
		input := s.bili.ExpandShortURL(req.Input)

		// 番剧：支持单集与批量 ep_ids 入队
		if pin, ok := ParsePgcInput(input); ok {
			if req.EpID > 0 {
				pin.EpID = req.EpID
			}
			info, err := s.bili.PgcView(pin)
			if err != nil {
				writeErr(w, 502, err)
				return
			}
			epSet := map[int64]bool{}
			for _, id := range req.EpIDs {
				if id > 0 {
					epSet[id] = true
				}
			}
			if len(epSet) == 0 && pin.EpID > 0 {
				epSet[pin.EpID] = true
			}
			var targets []PgcEpisode
			if len(epSet) > 0 {
				for _, ep := range info.Episodes {
					if epSet[ep.EpID] {
						targets = append(targets, ep)
					}
				}
			} else if len(info.Episodes) > 0 {
				targets = append(targets, info.Episodes[0])
			}
			if len(targets) == 0 {
				writeErr(w, 400, fmt.Errorf("未找到指定的剧集"))
				return
			}
			var created []*Task
			for _, ep := range targets {
				task, err := s.mgr.CreatePgc(info, ep, req.QN, codecID, req.AudioID)
				if err != nil {
					writeErr(w, 400, err)
					return
				}
				created = append(created, task)
			}
			writeJSON(w, 200, map[string]any{"task": created[0], "tasks": created, "count": len(created)})
			return
		}

		bvid, aid, page := ParseInput(input)
		info, err := s.bili.View(bvid, aid)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		pages := req.Pages
		if len(pages) == 0 {
			if req.Page > 0 {
				page = req.Page
			}
			pages = []int{page}
		}
		var created []*Task
		for _, p := range pages {
			task, err := s.mgr.Create(info, p, req.QN, codecID, req.AudioID)
			if err != nil {
				writeErr(w, 400, err)
				return
			}
			created = append(created, task)
		}
		writeJSON(w, 200, map[string]any{"task": created[0], "tasks": created, "count": len(created)})
	default:
		writeErr(w, 405, fmt.Errorf("不支持的方法"))
	}
}

type actionReq struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

func (s *Server) handleTaskAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("只支持 POST"))
		return
	}
	var req actionReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.mgr.Action(req.ID, req.Action); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleTaskFile(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	for _, t := range s.mgr.Snapshot() {
		if t.ID != id || t.OutputPath == "" {
			continue
		}
		f, err := os.Open(t.OutputPath)
		if err != nil {
			writeErr(w, 404, fmt.Errorf("文件不存在"))
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", urlEncode(filepath.Base(t.OutputPath))))
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, filepath.Base(t.OutputPath), st.ModTime(), f)
		return
	}
	writeErr(w, 404, fmt.Errorf("任务不存在或未完成"))
}

func urlEncode(s string) string {
	var sb strings.Builder
	for _, b := range []byte(s) {
		if (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '-' || b == '_' || b == '.' || b == '~' {
			sb.WriteByte(b)
		} else {
			sb.WriteString(fmt.Sprintf("%%%02X", b))
		}
	}
	return sb.String()
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, fmt.Errorf("不支持流式响应"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	enc := json.NewEncoder(w)
	for {
		payload := map[string]any{
			"tasks":    s.mgr.Snapshot(),
			"cooldown": s.bili.cooldownRemaining(),
			"ts":       time.Now().Unix(),
		}
		_, _ = w.Write([]byte("data: "))
		_ = enc.Encode(payload)
		_, _ = w.Write([]byte("\n"))
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
}

type settingsPayload struct {
	DownloadDir      string `json:"download_dir"`
	ChunkConcurrency int    `json:"chunk_concurrency"`
	BandwidthKB      int    `json:"bandwidth_kb"`
	Proxy            string `json:"proxy"`
	UserAgent        string `json:"user_agent"`
	FilenameTemplate string `json:"filename_template"`
	QN               int    `json:"qn"`
	VideoCodec       *int   `json:"video_codec"`
	AudioID          int    `json:"audio_id"`
	MaxTasks         int    `json:"max_tasks"`
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.store.Get()
		cookies := make([]string, 0, len(cfg.Cookies))
		for k := range cfg.Cookies {
			cookies = append(cookies, k)
		}
		sort.Strings(cookies)
		writeJSON(w, 200, map[string]any{
			"download_dir":       cfg.DownloadDir,
			"chunk_concurrency":  cfg.ChunkConcurrency,
			"bandwidth_kb":       cfg.BandwidthKB,
			"proxy":              cfg.Proxy,
			"user_agent":         cfg.UserAgent,
			"default_user_agent": defaultUserAgent,
			"filename_template":  cfg.FilenameTemplate,
			"qn":                 cfg.QN,
			"video_codec":        cfg.VideoCodec,
			"audio_id":           cfg.AudioID,
			"max_tasks":          cfg.MaxTasks,
			"cookie_names":       cookies,
		})
	case http.MethodPost:
		var p settingsPayload
		if err := readJSON(r, &p); err != nil {
			writeErr(w, 400, err)
			return
		}
		err := s.store.Update(func(c *Config) {
			if p.DownloadDir != "" {
				c.DownloadDir = p.DownloadDir
			}
			if p.ChunkConcurrency >= 1 && p.ChunkConcurrency <= 32 {
				c.ChunkConcurrency = p.ChunkConcurrency
			}
			if p.BandwidthKB >= 0 {
				c.BandwidthKB = p.BandwidthKB
			}
			if p.MaxTasks >= 1 && p.MaxTasks <= 10 {
				c.MaxTasks = p.MaxTasks
			}
			c.Proxy = strings.TrimSpace(p.Proxy)
			if ua := strings.TrimSpace(p.UserAgent); ua != "" {
				c.UserAgent = ua
			} else {
				c.UserAgent = defaultUserAgent
			}
			if strings.TrimSpace(p.FilenameTemplate) != "" {
				c.FilenameTemplate = p.FilenameTemplate
			}
			if p.QN > 0 {
				c.QN = p.QN
			}
			if p.VideoCodec != nil {
				c.VideoCodec = *p.VideoCodec
			}
			if p.AudioID > 0 {
				c.AudioID = p.AudioID
			}
		})
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		s.bili.applyConfig()
		s.mgr.applyRuntimeConfig(s.store.Get())
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeErr(w, 405, fmt.Errorf("不支持的方法"))
	}
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	dir := cfg.DownloadDir
	if q := r.URL.Query().Get("dir"); q != "" {
		if !filepath.IsAbs(q) {
			dir = filepath.Join(cfg.DownloadDir, q)
		} else {
			dir = q
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	type item struct {
		Name  string `json:"name"`
		Size  int64  `json:"size"`
		IsDir bool   `json:"is_dir"`
		Mod   int64  `json:"mod"`
	}
	out := make([]item, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		var size int64
		if err == nil {
			size = info.Size()
		}
		out = append(out, item{Name: e.Name(), Size: size, IsDir: e.IsDir(), Mod: info.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, 200, map[string]any{"dir": dir, "entries": out})
}

func lookupPort(cfg Config) int {
	if cfg.Port > 0 {
		return cfg.Port
	}
	return 18086
}
// handleCover 代理 B 站图片，绕过防盗链。
func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" || (!strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://")) {
		http.Error(w, "invalid url", 400)
		return
	}
	// B 站返回的图片地址常为 http，HTTPS 页面直接引用会被判混合内容；
	// 这里升级为 https 后再取，失败则回退原协议。
	if strings.HasPrefix(raw, "http://") {
		if d, c, e := s.bili.FetchImage("https://" + strings.TrimPrefix(raw, "http://")); e == nil {
			w.Header().Set("Content-Type", c)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(d)
			return
		}
	}
	data, ct, err := s.bili.FetchImage(raw)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}

// handleDirs 仅返回当前套件有写权限的共享文件夹候选。
func (s *Server) handleDirs(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	writeJSON(w, 200, map[string]any{
		"current":    cfg.DownloadDir,
		"candidates": listWritableCandidates(),
		"writable":   probeWritable(cfg.DownloadDir) == nil,
	})
}

type dirTestReq struct {
	Path string `json:"path"`
}

// handleDirTest 校验目录是否可写，用于设置页的「测试」按钮。
func (s *Server) handleDirTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("只支持 POST"))
		return
	}
	var req dirTestReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := probeWritable(req.Path); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	free, _ := freeGB(req.Path)
	writeJSON(w, 200, map[string]any{"ok": true, "message": "可以写入", "free_gb": free})
}

// summarizeStreams 保留每个 (画质ID, 编码ID) 组合，便于用户分别选择 AVC / HEVC / AV1。
func summarizeStreams(play PlayResult) ([]streamView, []streamView) {
	type vkey struct {
		id      int
		codecid int
	}
	vmap := map[vkey]StreamInfo{}
	for _, v := range play.Video {
		k := vkey{id: v.ID, codecid: v.Codecid}
		if old, ok := vmap[k]; !ok || v.Bandwidth > old.Bandwidth {
			vmap[k] = v
		}
	}
	vids := make([]streamView, 0, len(vmap))
	for _, v := range vmap {
		vids = append(vids, streamView{
			ID: v.ID, Label: v.Label, CodecLabel: CodecLabel(v.Codecid),
			Width: v.Width, Height: v.Height,
			Codecs: v.Codecs, Codecid: v.Codecid, Bandwidth: v.Bandwidth,
		})
	}
	sort.Slice(vids, func(i, j int) bool {
		if vids[i].ID != vids[j].ID {
			return vids[i].ID > vids[j].ID
		}
		if vids[i].Codecid != vids[j].Codecid {
			return vids[i].Codecid > vids[j].Codecid
		}
		return vids[i].Bandwidth > vids[j].Bandwidth
	})

	amap := map[int]StreamInfo{}
	for _, a := range play.Audio {
		if old, ok := amap[a.ID]; !ok || a.Bandwidth > old.Bandwidth {
			amap[a.ID] = a
		}
	}
	auds := make([]streamView, 0, len(amap))
	for _, a := range amap {
		auds = append(auds, streamView{ID: a.ID, Label: a.Label, Bandwidth: a.Bandwidth, Codecid: a.Codecid})
	}
	sort.Slice(auds, func(i, j int) bool { return auds[i].ID > auds[j].ID })
	return vids, auds
}

type epStreamsReq struct {
	Input   string `json:"input"`
	EpID    int64  `json:"ep_id"`
	SeasonID int64 `json:"season_id"`
}

// handleEpStreams 在番剧切换分集时刷新可用画质与音轨。
func (s *Server) handleEpStreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("只支持 POST"))
		return
	}
	var req epStreamsReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	pin := PgcInput{EpID: -1, SeasonID: -1, MediaID: -1}
	if req.EpID > 0 {
		pin.EpID = req.EpID
	}
	if req.SeasonID > 0 {
		pin.SeasonID = req.SeasonID
	}
	if pin.EpID <= 0 && pin.SeasonID <= 0 {
		expanded := s.bili.ExpandShortURL(req.Input)
		parsed, ok := ParsePgcInput(expanded)
		if !ok {
			writeErr(w, 400, fmt.Errorf("未识别到剧集"))
			return
		}
		pin = parsed
	}
	info, err := s.bili.PgcView(pin)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	var target *PgcEpisode
	for i := range info.Episodes {
		ep := info.Episodes[i]
		if pin.EpID > 0 && ep.EpID == pin.EpID {
			target = &info.Episodes[i]
			break
		}
		if pin.EpID <= 0 && target == nil {
			target = &info.Episodes[i]
		}
	}
	if target == nil {
		writeErr(w, 400, fmt.Errorf("未找到指定的剧集"))
		return
	}
	play, err := s.bili.PgcPlayURL(*target, 127)
	if err != nil {
		if errors.Is(err, ErrPreviewOnly) {
			writeErr(w, 409, err)
			return
		}
		writeErr(w, 502, err)
		return
	}
	vids, auds := summarizeStreams(play)
	cfg := s.store.Get()
	writeJSON(w, 200, map[string]any{
		"videos":   vids,
		"audios":   auds,
		"defaults": map[string]int{"qn": cfg.QN, "video_codec": cfg.VideoCodec, "audio_id": cfg.AudioID},
	})
}
