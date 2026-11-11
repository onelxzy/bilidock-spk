package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// 番剧/影视/课程（PGC）支持。
// 与普通视频的区别：
//   1. 详情走 pgc/view/web/season（按 season_id 或 ep_id 查询）
//   2. 播放地址走 pgc/player/web/v2/playurl，载荷在 result.video_info 内
//   3. Referer 必须是 bangumi/play/ep{id} 页面，否则会被拒绝
//   4. 需要先用 ep 确定具体一集，才能取到播放地址

// ErrPreviewOnly 表示该集只能试看，需要大会员或已购买。
var ErrPreviewOnly = errors.New("该集仅提供试看，需要大会员或购买后才能下载完整内容")

var (
	reEp  = regexp.MustCompile(`(?i)\bep(\d+)\b`)
	reSs  = regexp.MustCompile(`(?i)\bss(\d+)\b`)
	reMD  = regexp.MustCompile(`(?i)\bmd(\d+)\b`)
)

// PgcInput 是从用户输入解析出的 PGC 标识。
type PgcInput struct {
	EpID     int64
	SeasonID int64
	MediaID  int64
}

// ParsePgcInput 判断输入是否指向 PGC 内容（番剧/影视/课程）。
// 支持：ep123 / ss456 / md789、以及包含这些段的完整链接。
func ParsePgcInput(input string) (PgcInput, bool) {
	s := strings.TrimSpace(input)
	// 只看路径与查询，避免命中无关数字
	lower := strings.ToLower(s)
	if !strings.Contains(lower, "bangumi") && !strings.Contains(lower, "cheese") &&
		!strings.Contains(lower, "ep") && !strings.Contains(lower, "ss") && !strings.Contains(lower, "md") {
		return PgcInput{}, false
	}
	p := PgcInput{EpID: -1, SeasonID: -1, MediaID: -1}
	if m := reEp.FindStringSubmatch(s); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		p.EpID = n
	}
	if m := reSs.FindStringSubmatch(s); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		p.SeasonID = n
	}
	if m := reMD.FindStringSubmatch(s); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		p.MediaID = n
	}
	return p, p.EpID > -1 || p.SeasonID > -1 || p.MediaID > -1
}

// PgcEpisode 是番剧的一集。
type PgcEpisode struct {
	EpID      int64  `json:"ep_id"`
	Aid       int64  `json:"aid"`
	Bvid      string `json:"bvid"`
	Cid       int64  `json:"cid"`
	Title     string `json:"title"`
	LongTitle string `json:"long_title"`
	Cover     string `json:"cover"`
	Duration  int64  `json:"duration"`
	Badge     string `json:"badge"`
}

// PgcInfo 是番剧/影视详情。
type PgcInfo struct {
	SeasonID    int64        `json:"season_id"`
	MediaID     int64        `json:"media_id"`
	Title       string       `json:"title"`
	SeasonTitle string       `json:"season_title"`
	Cover       string       `json:"cover"`
	SquareCover string       `json:"square_cover"`
	Evaluate    string       `json:"evaluate"`
	TypeName    string       `json:"type_name"`
	Total       int          `json:"total"`
	Episodes    []PgcEpisode `json:"episodes"`
	IsPgc       bool         `json:"is_pgc"`
	EpIDHint    int64        `json:"ep_id_hint"`
}

var pgcTypeName = map[int64]string{
	1: "番剧", 2: "电影", 3: "纪录片", 4: "国创", 5: "电视剧", 7: "娱乐",
}

// resolveMediaToSeason 用 md 号换 season_id（与 DownKyi 的 pgc/review/user 等价路径）。
func (b *Bili) resolveMediaToSeason(mediaID int64) (int64, error) {
	params := url.Values{}
	params.Set("media_id", strconv.FormatInt(mediaID, 10))
	data, err := b.requestJSON("https://api.bilibili.com/pgc/review/user", params, false)
	if err != nil {
		return 0, err
	}
	if media, ok := data["media"].(map[string]any); ok {
		if sid := int64Of(media["season_id"]); sid > 0 {
			return sid, nil
		}
	}
	return 0, errors.New("无法从 md 号解析出 season_id")
}

// PgcView 获取番剧详情；season_id 优先，其次 ep_id，最后 md_id。
func (b *Bili) PgcView(in PgcInput) (PgcInfo, error) {
	params := url.Values{}
	switch {
	case in.SeasonID > 0:
		params.Set("season_id", strconv.FormatInt(in.SeasonID, 10))
	case in.EpID > 0:
		params.Set("ep_id", strconv.FormatInt(in.EpID, 10))
	case in.MediaID > 0:
		sid, err := b.resolveMediaToSeason(in.MediaID)
		if err != nil {
			return PgcInfo{}, err
		}
		params.Set("season_id", strconv.FormatInt(sid, 10))
	default:
		return PgcInfo{}, errors.New("未提供有效的 season/ep/md 号")
	}

	data, err := b.requestJSON("https://api.bilibili.com/pgc/view/web/season", params, false)
	if err != nil {
		return PgcInfo{}, err
	}
	// requestJSON 已把 result 通道内容解出，这里直接使用
	root := data
	info := PgcInfo{IsPgc: true, EpIDHint: in.EpID}
	info.SeasonID = int64Of(root["season_id"])
	info.MediaID = int64Of(root["media_id"])
	info.Title = stringOf(root["title"])
	info.SeasonTitle = stringOf(root["season_title"])
	info.Cover = stringOf(root["cover"])
	info.SquareCover = stringOf(root["square_cover"])
	info.Evaluate = stringOf(root["evaluate"])
	info.Total = int(int64Of(root["total"]))
	info.TypeName = pgcTypeName[int64Of(root["type"])]

	if eps, ok := root["episodes"].([]any); ok {
		for _, e := range eps {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			ep := PgcEpisode{
				EpID:      int64Of(m["ep_id"]),
				Aid:       int64Of(m["aid"]),
				Bvid:      stringOf(m["bvid"]),
				Cid:       int64Of(m["cid"]),
				Title:     stringOf(m["title"]),
				LongTitle: stringOf(m["long_title"]),
				Cover:     stringOf(m["cover"]),
				Duration:  int64Of(m["duration"]),
				Badge:     stringOf(m["badge"]),
			}
			// id 通常等于 ep_id，产品优先取 id（与 DownKyi 一致）
			if id := int64Of(m["id"]); id > 0 && ep.EpID == 0 {
				ep.EpID = id
			}
			if ep.EpID > 0 {
				info.Episodes = append(info.Episodes, ep)
			}
		}
	}
	// 正片之外的花絮/PV 章节
	if sections, ok := root["section"].([]any); ok {
		for _, s := range sections {
			sm, ok := s.(map[string]any)
			if !ok {
				continue
			}
			eps, ok := sm["episodes"].([]any)
			if !ok {
				continue
			}
			for _, e := range eps {
				m, ok := e.(map[string]any)
				if !ok {
					continue
				}
				ep := PgcEpisode{
					EpID:      int64Of(m["ep_id"]),
					Aid:       int64Of(m["aid"]),
					Bvid:      stringOf(m["bvid"]),
					Cid:       int64Of(m["cid"]),
					Title:     stringOf(m["title"]),
					LongTitle: stringOf(m["long_title"]),
					Cover:     stringOf(m["cover"]),
					Duration:  int64Of(m["duration"]),
				}
				if ep.EpID > 0 {
					info.Episodes = append(info.Episodes, ep)
				}
			}
		}
	}
	if len(info.Episodes) == 0 {
		return info, errors.New("该番剧没有可下载的分集（可能是地区限制或需要付费）")
	}
	return info, nil
}

// PgcPlayURL 获取番剧某一集的播放流。
// 注意：Referer 必须是 bangumi/play/ep{id}，否则 B 站会拒绝。
func (b *Bili) PgcPlayURL(ep PgcEpisode, qn int) (PlayResult, error) {
	if ep.EpID <= 0 {
		return PlayResult{}, errors.New("缺少 ep_id，无法获取番剧播放地址")
	}
	if qn <= 0 {
		qn = 127
	}
	params := url.Values{}
	params.Set("cid", strconv.FormatInt(ep.Cid, 10))
	params.Set("ep_id", strconv.FormatInt(ep.EpID, 10))
	params.Set("qn", strconv.Itoa(qn))
	params.Set("fourk", "1")
	params.Set("fnver", "0")
	params.Set("fnval", "4048")
	if ep.Bvid != "" {
		params.Set("bvid", ep.Bvid)
	} else if ep.Aid > 0 {
		params.Set("aid", strconv.FormatInt(ep.Aid, 10))
	}

	referer := fmt.Sprintf("https://www.bilibili.com/bangumi/play/ep%d", ep.EpID)
	data, err := b.requestJSONRef("https://api.bilibili.com/pgc/player/web/v2/playurl", params, false, referer)
	if err != nil {
		return PlayResult{}, err
	}
	root := data
	// 兼容仍以 result 包裹的响应
	if r, ok := data["result"].(map[string]any); ok {
		root = r
	}
	// 预览/受限检测
	if pc, ok := root["play_check"].(map[string]any); ok {
		if pd := stringOf(pc["play_detail"]); pd == "PLAY_PREVIEW" {
			return PlayResult{}, ErrPreviewOnly
		}
	}
	if pvt := stringOf(root["play_video_type"]); strings.EqualFold(pvt, "preview") {
		return PlayResult{}, ErrPreviewOnly
	}

	vi, ok := root["video_info"].(map[string]any)
	if !ok {
		return PlayResult{}, errors.New("该集播放信息不可用（可能需要大会员、未购买，或内容已下架）")
	}
	if pv, ok := vi["is_preview"].(bool); ok && pv {
		return PlayResult{}, ErrPreviewOnly
	}
	return parsePlayPayload(vi)
}

// parsePlayPayload 从 DASH/DURL 载荷中提取流列表。
// 普通视频与番剧共用这段逻辑。
func parsePlayPayload(data map[string]any) (PlayResult, error) {
	dash, _ := data["dash"].(map[string]any)
	res := PlayResult{}
	if dash == nil {
		// DURL 形态（老番/部分内容）：无 DASH，只能给一条合并流
		if durl, ok := data["durl"].([]any); ok && len(durl) > 0 {
			if first, ok := durl[0].(map[string]any); ok {
				if u := stringOf(first["url"]); u != "" {
					res.Video = append(res.Video, StreamInfo{
						ID:    int(int64Of(data["quality"])),
						URL:   u,
						Label: QualityLabel(int(int64Of(data["quality"]))),
						Kind:  "video",
					})
				}
			}
			if len(res.Video) > 0 {
				return res, nil
			}
		}
		return res, errors.New("该内容没有可用的 DASH 流")
	}
	addStreams := func(list []any, kind string) {
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			s := StreamInfo{
				ID:        int(int64Of(m["id"])),
				Codecid:   int(int64Of(m["codecid"])),
				URL:       stringOf(m["base_url"]),
				Bandwidth: int64Of(m["bandwidth"]),
				Width:     int(int64Of(m["width"])),
				Height:    int(int64Of(m["height"])),
				Kind:      kind,
			}
			if s.URL == "" {
				s.URL = stringOf(m["baseUrl"])
			}
			if bu, ok := m["backup_url"].([]any); ok {
				for _, u := range bu {
					if us, ok := u.(string); ok && us != "" {
						s.Backup = append(s.Backup, us)
					}
				}
			}
			if fr, ok := m["frame_rate"].(string); ok {
				s.FrameRate = fr
			}
			if c, ok := m["codecs"].(string); ok {
				s.Codecs = c
			}
			if kind == "video" {
				s.Label = QualityLabel(s.ID)
			} else {
				s.Label = AudioLabel(s.ID)
			}
			if s.URL == "" {
				continue
			}
			if kind == "video" {
				res.Video = append(res.Video, s)
			} else {
				res.Audio = append(res.Audio, s)
			}
		}
	}
	if list, ok := dash["video"].([]any); ok {
		addStreams(list, "video")
	}
	if list, ok := dash["audio"].([]any); ok {
		addStreams(list, "audio")
	}
	if dolby, ok := dash["dolby"].(map[string]any); ok {
		if list, ok := dolby["audio"].([]any); ok {
			addStreams(list, "audio")
		}
	}
	if flac, ok := dash["flac"].(map[string]any); ok {
		if a, ok := flac["audio"].(map[string]any); ok {
			addStreams([]any{a}, "audio")
		}
	}
	return res, nil
}