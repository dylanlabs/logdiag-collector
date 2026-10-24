// SPDX-License-Identifier: GPL-3.0-or-later

// Package hpe 是 HPE iLO 5、iLO 6 的 AHS（Active Health System）日志驱动。
//
// 流程依据 HPE 官方 python-ilorest-library 的示例 get_ahs_data.py 和 iLOrest 的 serverlogs 命令
// （Apache-2.0，只参考接口行为，不复制代码）：
//
//  1. 管理器的 Oem.Hpe.Links.ActiveHealthSystem 指向 AHS 资源（Gen9 的 iLO 4 是 Oem.Hp）。
//  2. 读 AHS 资源：Links.RecentWeek.extref 是最近 7 天的下载地址；固件没给时与 iLOrest 一样，
//     把 Links.AHSLocation.extref 里的 downloadAll=1 换成 from=<6 天前>&&to=<AHSFileEnd>。
//     不下载全部历史，那样文件可能有几百 MB。
//  3. 带令牌 GET 该地址，响应体就是 .ahs 文件。AHS 不需要设备端生成，没有等待。
//  4. 顺带读 IML（系统的日志服务）和 IEL（管理器的日志服务），作为附带文件 IML.json、IEL.json。
//     用 $expand=. 一次读出条目内容；iLO 不支持展开或读取失败时跳过，不影响 AHS。
package hpe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// RecentDays 是下载的天数，与 iLO 的 RecentWeek 和 iLOrest 的默认一致。
const RecentDays = 7

// MaxPages 是读一个日志条目集合最多翻的页数，防止固件给出循环的下一页地址。
const MaxPages = 50

// Driver 是 iLO 驱动。
type Driver struct{}

func (Driver) Name() string { return "hpe-ilo" }

// ahs 是 AHS 资源里用到的字段。
type ahs struct {
	Links        json.RawMessage
	AHSFileStart string // AHS 里最早数据的时间
	AHSFileEnd   string // 最新数据的时间
	Status       struct{ State string }
}

func (Driver) Collect(ctx context.Context, j *collect.Job) (*collect.Result, error) {
	if j.Redfish == nil || j.Manager == nil {
		return nil, fmt.Errorf("iLO 需要 Redfish 接口：%w", redfish.ErrUnsupported)
	}
	if j.Device.Controller.Type == "" {
		j.Device.Controller.Type = "iLO"
		if m := strings.TrimSpace(j.Manager.Model); strings.HasPrefix(m, "iLO") {
			j.Device.Controller.Type = m // 例如 iLO 6
		}
	}
	log := collect.Log{Type: "hpe.ilo.ahs", Method: "redfish-oem"}

	ahsURI := j.Manager.OemLink("Links", "ActiveHealthSystem")
	if ahsURI == "" {
		return nil, unsupported(fmt.Errorf("管理器 %s 没有 ActiveHealthSystem 链接：%w", j.Manager.ODataID, redfish.ErrUnsupported))
	}
	j.Report(collect.StageDownload, -1, "读取 AHS 下载地址")
	var a ahs
	if _, err := j.Redfish.Get(ctx, ahsURI, &a); err != nil {
		return nil, fmt.Errorf("读取 AHS：%w", err)
	}
	switch a.Status.State {
	case "InUse", "Unavailable", "UnavailableOffline":
		return nil, redfish.WithHint(fmt.Errorf("AHS 当前不可用（%s）", a.Status.State), redfish.KindBusy,
			"AHS 日志正被其他程序使用（例如有人在 iLO 网页上下载），稍后再试。")
	}
	now := j.Now()
	file := recentURI(a, now)
	if file == "" {
		return nil, unsupported(fmt.Errorf("%s 没有给出 AHS 下载地址：%w", ahsURI, redfish.ErrUnsupported))
	}
	log.Range = rangeOf(file, now)

	var extras []collect.Attachment
	if j.System != nil {
		if att, ok := logEntries(ctx, j, j.System.LogServices.ID, "IML"); ok {
			extras = append(extras, att)
		}
	}
	if att, ok := logEntries(ctx, j, j.Manager.LogServices.ID, "IEL"); ok {
		extras = append(extras, att)
	}

	name := path.Base(strings.SplitN(file, "?", 2)[0])
	if !strings.HasSuffix(strings.ToLower(name), ".ahs") {
		name = fmt.Sprintf("HPE_%s_%s.ahs", strings.TrimSpace(j.Device.Serial), now.Format("20060102"))
	}
	j.Report(collect.StageDownload, -1, fmt.Sprintf("下载最近 %d 天的 AHS 日志（%s）", RecentDays, name))
	return j.Save(log, name, func(w io.Writer) error {
		if _, _, err := j.Redfish.Download(ctx, file, w); err != nil {
			return fmt.Errorf("下载 AHS 日志：%w", err)
		}
		return nil
	}, extras...)
}

// recentURI 返回最近 7 天的 AHS 下载地址，没有下载地址时为空。
func recentURI(a ahs, now time.Time) string {
	if week := redfish.Link(a.Links, "RecentWeek"); week != "" {
		return week
	}
	all := redfish.Link(a.Links, "AHSLocation")
	if all == "" {
		return ""
	}
	// 与 iLOrest 相同：起点是 6 天前，但不早于 AHS 最早的数据、不晚于最新的数据；终点是最新数据那天。
	today := day(now)
	from, to := today.AddDate(0, 0, -(RecentDays-1)), today
	start, okStart := parseDay(a.AHSFileStart, now.Location())
	end, okEnd := parseDay(a.AHSFileEnd, now.Location())
	if okStart && okEnd {
		from, to = minTime(maxTime(from, start), end), end
	}
	base, _, _ := strings.Cut(all, "downloadAll=1")
	switch {
	case !strings.Contains(base, "?"):
		base += "?"
	case !strings.HasSuffix(base, "?") && !strings.HasSuffix(base, "&"):
		base += "&"
	}
	return base + "from=" + from.Format(time.DateOnly) + "&&to=" + to.Format(time.DateOnly)
}

// rangeOf 从下载地址的 from/to（或 days）参数得出 manifest 的 log.range，看不出时为空。
func rangeOf(uri string, now time.Time) collect.Range {
	_, query, _ := strings.Cut(uri, "?")
	q, _ := url.ParseQuery(query) // iLO 用 && 分隔参数，空的一段会被跳过
	loc := now.Location()
	from, okFrom := parseDay(q.Get("from"), loc)
	to, okTo := parseDay(q.Get("to"), loc)
	if n, err := strconv.Atoi(q.Get("days")); err == nil && n > 0 && !okFrom && !okTo {
		to, okTo = day(now), true
		from, okFrom = to.AddDate(0, 0, -(n-1)), true
	}
	if !okFrom || !okTo || to.Before(from) {
		return collect.Range{}
	}
	return collect.Range{
		From: from.Format(time.RFC3339),
		To:   to.Add(24*time.Hour - time.Second).Format(time.RFC3339),
	}
}

// logEntries 读日志服务 id（IML 或 IEL）的全部条目，返回附带文件 <id>.json：
// {"@odata.id": 条目集合地址, "Members@odata.count": 条数, "Members": [完整的日志条目]}。
func logEntries(ctx context.Context, j *collect.Job, services, id string) (collect.Attachment, bool) {
	if services == "" {
		return collect.Attachment{}, false
	}
	skip := func(reason string) (collect.Attachment, bool) {
		j.Report(collect.StageDownload, -1, "跳过 "+id+" 日志："+reason)
		return collect.Attachment{}, false
	}
	j.Report(collect.StageDownload, -1, "读取 "+id+" 日志")
	svc := strings.TrimSuffix(services, "/") + "/" + id
	var ls struct{ Entries redfish.ODataID }
	if _, err := j.Redfish.Get(ctx, svc, &ls); err != nil {
		return skip(err.Error())
	}
	entries := ls.Entries.ID
	if entries == "" {
		entries = svc + "/Entries"
	}
	members := []json.RawMessage{}
	next := withExpand(entries)
	for page := 0; next != ""; page++ {
		if page >= MaxPages {
			j.Report(collect.StageDownload, -1, fmt.Sprintf("%s 日志超过 %d 页，只保存前 %d 条", id, MaxPages, len(members)))
			break
		}
		var coll struct {
			Members  []json.RawMessage
			NextLink string `json:"Members@odata.nextLink"`
		}
		if _, err := j.Redfish.Get(ctx, next, &coll); err != nil {
			return skip(err.Error())
		}
		for _, m := range coll.Members {
			var fields map[string]json.RawMessage
			if json.Unmarshal(m, &fields) != nil || len(fields) <= 1 {
				return skip("iLO 不支持一次读出条目内容（$expand）")
			}
		}
		members = append(members, coll.Members...)
		next = ""
		if coll.NextLink != "" {
			next = withExpand(coll.NextLink)
		}
	}
	data, err := json.MarshalIndent(struct {
		ODataID string            `json:"@odata.id"`
		Count   int               `json:"Members@odata.count"`
		Members []json.RawMessage `json:"Members"`
	}{entries, len(members), members}, "", "  ")
	if err != nil {
		return skip(err.Error())
	}
	return collect.Attachment{Name: id + ".json", Data: append(data, '\n')}, true
}

// withExpand 给集合地址加上 $expand=.，让 iLO 在一页里返回条目内容而不只是链接。
func withExpand(uri string) string {
	if strings.Contains(uri, "$expand") {
		return uri
	}
	if strings.Contains(uri, "?") {
		return uri + "&$expand=."
	}
	return uri + "?$expand=."
}

// day 返回 t 那天的零点。
func day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// parseDay 解析 2026-10-26 或 2026-10-26T08:00:00Z 里的日期部分。
func parseDay(s string, loc *time.Location) (time.Time, bool) {
	if len(s) < len(time.DateOnly) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(time.DateOnly, s[:len(time.DateOnly)], loc)
	return t, err == nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func unsupported(err error) error {
	return redfish.WithHint(err, redfish.KindUnsupported,
		"这台 iLO 没有提供 AHS 下载接口，可先升级 iLO 固件再试。")
}
