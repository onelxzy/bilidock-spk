package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Version 由构建时注入，默认值用于本地调试。
var Version = "1.0.0-dev"

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
}

// resolveVarDir 依次尝试：命令行 > 环境变量 > 套件标准路径 > 可执行文件同级 var。
func resolveVarDir(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("BILIDOCK_VAR"); v != "" {
		return v
	}
	const pkgVar = "/var/packages/bilidock/var"
	if st, err := os.Stat(pkgVar); err == nil && st.IsDir() {
		return pkgVar
	}
	return filepath.Join(exeDir(), "var")
}

// findFFmpeg 优先使用套件内置的 ffmpeg，其次回落到 PATH。
func findFFmpeg() string {
	candidates := []string{
		filepath.Join(exeDir(), "ffmpeg"),
		filepath.Join(exeDir(), "bin", "ffmpeg"),
		filepath.Join(filepath.Dir(exeDir()), "bin", "ffmpeg"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if p, err := lookPath("ffmpeg"); err == nil {
		return p
	}
	return "ffmpeg"
}

func lookPath(name string) (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in PATH", name)
}

// portAvailable 检测回环端口是否可绑定。
func portAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// pickPort 返回可用端口：优先使用配置端口，被占用时顺延搜索。
// 结果会写回配置，保证 CGI 代理能连到实际监听端口。
func pickPort(store *ConfigStore, cfg Config, override int) int {
	if override > 0 {
		return override
	}
	want := cfg.Port
	if want <= 0 {
		want = defaultPort
	}
	if portAvailable(want) {
		return want
	}
	log.Printf("端口 %d 已被占用，正在自动选择其他可用端口", want)
	for p := want + 1; p < want+200; p++ {
		if portAvailable(p) {
			log.Printf("已切换到可用端口 %d", p)
			_ = store.Update(func(c *Config) { c.Port = p })
			return p
		}
	}
	log.Printf("未找到可用端口，将尝试原端口 %d", want)
	return want
}

func main() {
	var (
		cgiMode = flag.Bool("cgi", false, "以 CGI 方式运行（DSM 网页服务器调用）")
		varDir  = flag.String("var", "", "数据目录，默认 /var/packages/bilidock/var")
		port    = flag.Int("port", 0, "监听端口，默认取配置文件")
		showVer = flag.Bool("version", false, "显示版本")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("bilidock", Version)
		return
	}

	dir := resolveVarDir(*varDir)
	store, err := newConfigStore(dir)
	if err != nil {
		log.Fatalf("初始化数据目录失败: %v", err)
	}
	cfg := store.Get()

	if *cgiMode || os.Getenv("GATEWAY_INTERFACE") != "" {
		p := cfg.Port
		if *port > 0 {
			p = *port
		}
		runCGI(p)
		return
	}

	logDir := filepath.Join(dir, "logs")
	_ = os.MkdirAll(logDir, 0o700)
	logFile, err := os.OpenFile(filepath.Join(logDir, "bilidock.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		defer logFile.Close()
		log.SetOutput(logFile)
	}
	log.SetFlags(log.LstdFlags)

	if cfg.DownloadDir == "" {
		def := os.Getenv("BILIDOCK_DEFAULT_DIR")
		if def == "" {
			def = filepath.Join(dir, "downloads")
		}
		if probeWritable(def) != nil {
			def = filepath.Join(dir, "downloads")
		}
		cfg.DownloadDir = def
		_ = store.Update(func(c *Config) { c.DownloadDir = cfg.DownloadDir })
	}
	if probeWritable(cfg.DownloadDir) != nil {
		log.Printf("警告: 下载目录 %s 当前不可写，请在界面设置中改到可写的共享文件夹", cfg.DownloadDir)
	} else {
		_ = os.MkdirAll(cfg.DownloadDir, 0o755)
	}

	bili := newBili(store)
	mgr := newManager(store, bili, dir, findFFmpeg())
	mgr.StartSpeedTicker()

	srv := NewServer(store, bili, mgr, dir)
	listenPort := pickPort(store, cfg, *port)
	addr := fmt.Sprintf("127.0.0.1:%d", listenPort)
	log.Printf("bilidock %s 启动，监听 %s，数据目录 %s", Version, addr, dir)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(httpServer.ListenAndServe())
}