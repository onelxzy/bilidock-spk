package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var hopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// runCGI 把 DSM Web 服务器转交的 CGI 请求代理到本地常驻服务。
// 这样界面与 API 全部运行在 DSM 自己的 5000/5001 端口上，不需要额外端口。
func runCGI(port int) {
	path := os.Getenv("PATH_INFO")
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	method := os.Getenv("REQUEST_METHOD")
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if n, err := strconv.Atoi(os.Getenv("CONTENT_LENGTH")); err == nil && n > 0 {
		body = io.LimitReader(os.Stdin, int64(n))
	}

	// 只访问 127.0.0.1：显式禁用所有代理。
	// CGI 环境常带有 HTTP_PROXY/HTTPS_PROXY（可能来自客户端的 Proxy 头），
	// Go 在 CGI 环境下会直接拒绝使用它们，导致转发失败。
	client := &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy:               nil,
			MaxIdleConns:        4,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	// 端口可能因冲突自动顺延，因此依次尝试配置端口与其后若干端口，
	// 取第一个真正在监听的服务。
	ports := []int{port}
	for i := 1; i <= 30; i++ {
		ports = append(ports, port+i)
	}

	var lastErr error
	for _, p := range ports {
		target := fmt.Sprintf("http://127.0.0.1:%d%s", p, path)
		if qs := os.Getenv("QUERY_STRING"); qs != "" {
			target += "?" + qs
		}
		req, err := http.NewRequest(method, target, body)
		if err != nil {
			cgiFail(502, err)
			return
		}
		if ct := os.Getenv("CONTENT_TYPE"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if ua := os.Getenv("HTTP_USER_AGENT"); ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			// 该端口无服务，换下一个端口重试（请求体需可重读，普通 API 为小 JSON）
			if body != nil {
				if n, e2 := strconv.Atoi(os.Getenv("CONTENT_LENGTH")); e2 == nil && n > 0 {
					body = io.LimitReader(os.Stdin, int64(n))
				}
			}
			continue
		}
		writeCGIResponse(resp)
		return
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("本地服务未响应")
	}
	cgiFail(502, lastErr)
}

// writeCGIResponse 把后端响应转换为 CGI 输出。
func writeCGIResponse(resp *http.Response) {
	defer resp.Body.Close()
	fmt.Printf("Status: %d %s\r\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	for k, vs := range resp.Header {
		if hopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			fmt.Printf("%s: %s\r\n", k, v)
		}
	}
	fmt.Print("\r\n")
	_, _ = io.Copy(os.Stdout, resp.Body)
}

func cgiFail(code int, err error) {
	if code == 502 {
		fmt.Printf("Status: 502 Bad Gateway\r\nContent-Type: text/html; charset=utf-8\r\n\r\n")
		fmt.Printf(`<!doctype html><html><head><meta charset="utf-8"><title>服务未就绪</title>
<style>body{font:15px/1.7 system-ui,"Segoe UI",sans-serif;max-width:640px;margin:80px auto;padding:0 20px;color:#333}
code{background:#f3f4f6;padding:2px 6px;border-radius:4px}</style></head><body>
<h2>后台服务未运行</h2>
<p>界面需要套件内的常驻服务配合。请确认：</p>
<ol><li>套件已启动（套件中心 → 已安装 → 运行中）</li>
<li>查看日志 <code>/var/packages/bilidock/target/logs/bilidock.log</code></li></ol>
<p style="color:#888;font-size:13px">%s · %s</p></body></html>`, err.Error(), time.Now().Format("15:04:05"))
		return
	}
	fmt.Printf("Status: %d Error\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\n", code, err.Error())
}