package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type QRInfo struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

type LoginState struct {
	LoggedIn bool   `json:"logged_in"`
	Mid      int64  `json:"mid"`
	Name     string `json:"name"`
	Face     string `json:"face"`
	VIP      bool   `json:"vip"`
	VIPLabel string `json:"vip_label"`
	Level    int    `json:"level"`
}

type PageInfo struct {
	Cid      int64  `json:"cid"`
	Page     int    `json:"page"`
	Part     string `json:"part"`
	Duration int64  `json:"duration"`
}

type VideoInfo struct {
	Bvid     string     `json:"bvid"`
	Aid      int64      `json:"aid"`
	Title    string     `json:"title"`
	Cover    string     `json:"cover"`
	Owner    string     `json:"owner"`
	Face     string     `json:"face"`
	Duration int64      `json:"duration"`
	Pubdate  int64      `json:"pubdate"`
	Desc     string     `json:"desc"`
	Cid      int64      `json:"cid"`
	Pages    []PageInfo `json:"pages"`
}

type StreamInfo struct {
	ID        int      `json:"id"`
	Codecid   int      `json:"codecid"`
	URL       string   `json:"url"`
	Backup    []string `json:"-"`
	Bandwidth int64    `json:"bandwidth"`
	Width     int      `json:"width"`
	Height    int      `json:"height"`
	FrameRate string   `json:"frame_rate"`
	Codecs    string   `json:"codecs"`
	Label     string   `json:"label"`
	Kind      string   `json:"kind"`
}

type PlayResult struct {
	Video []StreamInfo `json:"video"`
	Audio []StreamInfo `json:"audio"`
}

var (
	reBV = regexp.MustCompile(`(?i)\bBV[0-9A-Za-z]{10}\b`)
	reAV = regexp.MustCompile(`(?i)\bav(\d+)\b`)
	reEP = regexp.MustCompile(`(?i)\bep(\d+)\b`)
)

var (
	rePage    = regexp.MustCompile(`[?&]p=(\d+)`)
	reShortB23 = regexp.MustCompile(`https?://(?:b23\.tv|bili2233\.cn)/[0-9A-Za-z]+`)
)

// ExpandShortURL 把 b23.tv 短链展开为真实目标 URL；不是短链或展开失败时原样返回。
func (b *Bili) ExpandShortURL(input string) string {
	m := reShortB23.FindString(input)
	if m == "" {
		return input
	}
	base := b.httpClient()
	client := &http.Client{
		Transport: base.Transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest("GET", m, nil)
	if err != nil {
		return input
	}
	req.Header.Set("User-Agent", b.userAgent())
	resp, err := client.Do(req)
	if err != nil {
		return input
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "" {
		return loc
	}
	return input
}

var qualityLabels = map[int]string{
	6:   "240P 极速",
	16:  "360P 流畅",
	32:  "480P 清晰",
	64:  "720P 高清",
	74:  "720P 60帧",
	80:  "1080P 高清",
	112: "1080P 高码率",
	116: "1080P 60帧",
	120: "4K 超清",
	125: "HDR 真彩",
	126: "杜比视界",
	127: "8K 超高清",
}

var audioLabels = map[int]string{
	30216: "64K 流畅",
	30232: "132K 标准",
	30280: "192K 高码率",
	30250: "杜比全景声",
	30251: "Hi-Res 无损",
}

func QualityLabel(id int) string {
	if s, ok := qualityLabels[id]; ok {
		return s
	}
	return "画质 " + strconv.Itoa(id)
}

func AudioLabel(id int) string {
	if s, ok := audioLabels[id]; ok {
		return s
	}
	return "音频 " + strconv.Itoa(id)
}

func CodecLabel(codecid int) string {
	switch codecid {
	case 7:
		return "AVC"
	case 12:
		return "HEVC"
	case 13:
		return "AV1"
	default:
		return "自动"
	}
}

// QRGenerate 申请一个登录二维码。
func (b *Bili) QRGenerate() (QRInfo, error) {
	body, _, err := b.doRaw("https://passport.bilibili.com/x/passport-login/web/qrcode/generate", false, nil)
	if err != nil {
		return QRInfo{}, err
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			URL string `json:"url"`
			Key string `json:"qrcode_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return QRInfo{}, err
	}
	if env.Code != 0 {
		return QRInfo{}, &BiliError{Code: env.Code, Message: env.Msg}
	}
	return QRInfo{URL: env.Data.URL, Key: env.Data.Key}, nil
}

// QRPoll 轮询登录状态，成功时落盘 cookie。返回状态码与原样提示。
func (b *Bili) QRPoll(key string) (int, string, error) {
	u := "https://passport.bilibili.com/x/passport-login/web/qrcode/poll?qrcode_key=" + url.QueryEscape(key)
	body, hdr, err := b.doRaw(u, false, nil)
	if err != nil {
		return -1, "", err
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return -1, "", err
	}
	if env.Code != 0 {
		return env.Code, env.Msg, nil
	}
	if env.Data.Code != 0 {
		return env.Data.Code, env.Data.Message, nil
	}

	cookies := map[string]string{}
	collectFromHeader(cookies, hdr)
	if env.Data.URL != "" {
		if pu, perr := url.Parse(env.Data.URL); perr == nil {
			for k, vs := range pu.Query() {
				if len(vs) > 0 && isAuthCookie(k) {
					cookies[k] = vs[0]
				}
			}
		}
	}
	if len(cookies) > 0 {
		_ = b.store.Update(func(c *Config) {
			for k, v := range cookies {
				c.Cookies[k] = v
			}
		})
	}
	return 0, "登录成功", nil
}

func isAuthCookie(name string) bool {
	switch name {
	case "SESSDATA", "bili_jct", "DedeUserID", "DedeUserID__ckMd5", "sid":
		return true
	}
	return false
}

func collectFromHeader(dst map[string]string, hdr map[string][]string) {
	for _, line := range hdr["Set-Cookie"] {
		parts := strings.Split(line, ";")
		if len(parts) == 0 {
			continue
		}
		kv := strings.SplitN(strings.TrimSpace(parts[0]), "=", 2)
		if len(kv) == 2 && isAuthCookie(kv[0]) && kv[1] != "" && kv[1] != "deleted" {
			dst[kv[0]] = kv[1]
		}
	}
}

// Nav 查询当前登录状态与大会员信息。
func (b *Bili) Nav() (LoginState, error) {
	data, err := b.requestJSON("https://api.bilibili.com/x/web-interface/nav", nil, false)
	if err != nil {
		return LoginState{}, err
	}
	st := LoginState{}
	if v, ok := data["isLogin"].(bool); ok {
		st.LoggedIn = v
	}
	if !st.LoggedIn {
		return st, nil
	}
	st.Mid = int64Of(data["mid"])
	st.Name = stringOf(data["uname"])
	st.Face = stringOf(data["face"])
	if li, ok := data["level_info"].(map[string]any); ok {
		st.Level = int(int64Of(li["current_level"]))
	}
	if vs, ok := data["vipStatus"].(float64); ok && vs == 1 {
		st.VIP = true
	}
	if vd, ok := data["vip_label"].(map[string]any); ok {
		st.VIPLabel = stringOf(vd["text"])
	}
	if st.VIPLabel == "" {
		if vip, ok := data["vip"].(map[string]any); ok {
			if vt, ok := vip["label"].(map[string]any); ok {
				st.VIPLabel = stringOf(vt["text"])
			}
		}
	}
	if st.VIPLabel != "" {
		st.VIP = true
	}
	return st, nil
}

// ParseInput 从用户输入中提取 BV/av 与分 P。
func ParseInput(input string) (string, int64, int) {
	page := 1
	if m := rePage.FindStringSubmatch(input); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			page = n
		}
	}
	if m := reBV.FindString(input); m != "" {
		return m, 0, page
	}
	if m := reAV.FindStringSubmatch(input); m != nil {
		aid, _ := strconv.ParseInt(m[1], 10, 64)
		return "", aid, page
	}
	return strings.TrimSpace(input), 0, page
}

// View 获取普通视频的基础信息与分 P 列表。
func (b *Bili) View(bvid string, aid int64) (VideoInfo, error) {
	params := url.Values{}
	if bvid != "" {
		params.Set("bvid", bvid)
	} else if aid > 0 {
		params.Set("aid", strconv.FormatInt(aid, 10))
	} else {
		return VideoInfo{}, errors.New("未识别到有效的 BV 号或 av 号")
	}
	data, err := b.requestJSON("https://api.bilibili.com/x/web-interface/view", params, false)
	if err != nil {
		return VideoInfo{}, err
	}
	info := VideoInfo{}
	info.Bvid = stringOf(data["bvid"])
	info.Aid = int64Of(data["aid"])
	info.Title = stringOf(data["title"])
	info.Cover = stringOf(data["pic"])
	info.Desc = stringOf(data["desc"])
	info.Duration = int64Of(data["duration"])
	info.Pubdate = int64Of(data["pubdate"])
	if owner, ok := data["owner"].(map[string]any); ok {
		info.Owner = stringOf(owner["name"])
		info.Face = stringOf(owner["face"])
	}
	info.Cid = int64Of(data["cid"])
	if pages, ok := data["pages"].([]any); ok {
		for _, p := range pages {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			info.Pages = append(info.Pages, PageInfo{
				Cid:      int64Of(pm["cid"]),
				Page:     int(int64Of(pm["page"])),
				Part:     stringOf(pm["part"]),
				Duration: int64Of(pm["duration"]),
			})
		}
	}
	if len(info.Pages) > 0 && info.Cid == 0 {
		info.Cid = info.Pages[0].Cid
	}
	if info.Title == "" {
		return VideoInfo{}, errors.New("未获取到视频信息，请确认链接有效")
	}
	return info, nil
}

// PlayURL 获取普通视频的 DASH 流列表；fnval=4048 覆盖 8K/HDR/杜比/AV1。
func (b *Bili) PlayURL(bvid string, aid, cid int64, qn int) (PlayResult, error) {
	params := url.Values{}
	if bvid != "" {
		params.Set("bvid", bvid)
	} else if aid > 0 {
		params.Set("avid", strconv.FormatInt(aid, 10))
	}
	params.Set("cid", strconv.FormatInt(cid, 10))
	params.Set("qn", strconv.Itoa(qn))
	params.Set("fnver", "0")
	params.Set("fnval", "4048")
	params.Set("fourk", "1")
	data, err := b.requestJSON("https://api.bilibili.com/x/player/wbi/playurl", params, true)
	if err != nil {
		return PlayResult{}, err
	}
	return parsePlayPayload(data)
}

// PickStreams 从可用流中挑出目标画质、编码与音轨，不可用时自动向下/备用回退。
func PickStreams(res PlayResult, qn, codecID, audioID int) (*StreamInfo, *StreamInfo) {
	var video *StreamInfo
	candidates := res.Video
	if codecID > 0 {
		var matched []StreamInfo
		for _, v := range res.Video {
			if v.Codecid == codecID {
				matched = append(matched, v)
			}
		}
		if len(matched) > 0 {
			candidates = matched
		}
	}
	vids := append([]StreamInfo{}, candidates...)
	sort.Slice(vids, func(i, j int) bool {
		if vids[i].ID != vids[j].ID {
			return vids[i].ID > vids[j].ID
		}
		return vids[i].Bandwidth > vids[j].Bandwidth
	})
	for i := range vids {
		if vids[i].ID == qn {
			video = &vids[i]
			break
		}
	}
	if video == nil {
		for i := range vids {
			if vids[i].ID <= qn {
				video = &vids[i]
				break
			}
		}
	}
	if video == nil && len(vids) > 0 {
		video = &vids[0]
	}

	var audio *StreamInfo
	auds := append([]StreamInfo{}, res.Audio...)
	sort.Slice(auds, func(i, j int) bool { return auds[i].ID > auds[j].ID })
	for i := range auds {
		if auds[i].ID == audioID {
			audio = &auds[i]
			break
		}
	}
	if audio == nil && len(auds) > 0 {
		audio = &auds[0]
	}
	return video, audio
}

func int64Of(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

func stringOf(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return fmt.Sprint(v)
}