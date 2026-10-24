// SPDX-License-Identifier: GPL-3.0-or-later

package drivers

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/collect/dell"
	"github.com/dylanlabs/logdiag-collector/internal/collect/lenovo"
	"github.com/dylanlabs/logdiag-collector/internal/httprec"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// 这里的录制记录都是按官方文档和官方脚本合成的，note 字段写明了依据，不是真机验证结果。

const (
	dellLC   = "/redfish/v1/Managers/iDRAC.Embedded.1/Oem/Dell/DellLCService"
	dellSA   = dellLC + "/Actions/DellLCService.SupportAssistCollection"
	dellEULA = dellLC + "/Actions/DellLCService.SupportAssistAcceptEULA"
	dellJob  = "/redfish/v1/Managers/iDRAC.Embedded.1/Jobs/JID_612345678901"
)

func TestDellTSR(t *testing.T) {
	r := newReplay(t, "dell_idrac9_tsr.json")
	res, err := Collect(context.Background(), r.req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// 序列号用服务标签（SKU），不用主板序列号。
	if filepath.Base(res.Dir) != "Dell_PowerEdgeR750_SYNTH01_tsr_20261026-1403" || filepath.Base(res.Path) != "sacollect.zip" {
		t.Errorf("Path = %q", res.Path)
	}
	m := res.Manifest
	if m.Log != (collect.Log{Type: "dell.idrac.tsr", Method: "redfish-oem"}) ||
		m.Device.Vendor != "Dell" || m.Device.Model != "PowerEdge R750" || m.Device.Serial != "SYNTH01" || m.Device.Hostname != "esx01" ||
		m.Device.Controller != (collect.Controller{Type: "iDRAC", Firmware: "7.10.70.00"}) {
		t.Errorf("Manifest = %+v", m)
	}
	if len(m.Files) != 1 || m.Files[0].Size != 502 || m.Files[0].SHA256 != "5a312a6654217eff98eeedc6f55c13bcae9503161219ca1bf02ce2d19a7c90ec" {
		t.Errorf("Files = %+v", m.Files)
	}

	sa := r.cap.find("POST", dellSA)
	if len(sa) != 1 || string(sa[0].body) != `{"DataSelectorArrayIn":["HWData","TTYLogs"],"ShareType":"Local"}` {
		t.Errorf("SupportAssistCollection 请求 = %q", sa)
	}
	if len(r.cap.find("POST", dellEULA)) != 0 {
		t.Error("设备没要求时不应接受许可协议")
	}
	if len(r.cap.find("DELETE", "/redfish/v1/SessionService/Sessions/27")) != 1 {
		t.Error("结束后应删除会话")
	}
	want := []collect.Stage{collect.StageProbe, collect.StageTrigger, collect.StageGenerate,
		collect.StageDownload, collect.StageVerify, collect.StageDone}
	if got := r.stages(); !slices.Equal(got, want) {
		t.Errorf("阶段 = %v，应为 %v", got, want)
	}
	// 进度来自 Dell Job 的 PercentComplete 和 Message。
	if !slices.ContainsFunc(r.events, func(e collect.Event) bool {
		return e.Stage == collect.StageGenerate && e.Percent == 50 && e.Message == "Collecting hardware data."
	}) {
		t.Errorf("没有报告 50%% 的进度：%v", r.events)
	}
	if want := []time.Duration{5 * time.Second, 5 * time.Second}; !slices.Equal(r.waits, want) {
		t.Errorf("等待 = %v，应为 %v", r.waits, want)
	}
}

// TestDellTaskServiceFallback：Job 完成时不给文件地址，按老版官方脚本再查 TaskService/Tasks/<JobID>。
func TestDellTaskServiceFallback(t *testing.T) {
	c := loadCassette(t, "dell_idrac9_tsr.json")
	for _, it := range c.Interactions {
		if it.Request.Path == dellJob {
			it.Response.Header.Del("Location")
		}
	}
	c.Interactions = append(c.Interactions, &httprec.Interaction{
		Request: httprec.Request{Method: "GET", Path: "/redfish/v1/TaskService/Tasks/JID_612345678901"},
		Response: httprec.Response{
			Status: 200,
			Header: http.Header{"Content-Type": {"application/json"}, "Location": {"/redfish/v1/Dell/sacollect.zip"}},
			Body:   httprec.Body{JSON: json.RawMessage(`{"Id":"JID_612345678901","TaskState":"Completed","PercentComplete":100}`)},
		},
	})
	r := newReplayFrom(t, c)
	res, err := Collect(context.Background(), r.req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if filepath.Base(res.Path) != "sacollect.zip" {
		t.Errorf("Path = %q", res.Path)
	}
}

func TestDellAcceptEULA(t *testing.T) {
	r := newReplay(t, "dell_idrac10_eula.json")
	r.req.AcceptEULA = true
	res, err := Collect(context.Background(), r.req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	m := res.Manifest
	if m.Device.Serial != "SYNTH10" || m.Device.Controller != (collect.Controller{Type: "iDRAC", Firmware: "1.20.25.00"}) ||
		m.Files[0].Size != 242 || m.Files[0].SHA256 != "3a969c4f4e46709aa340f0692823955e639612486540a91ea92ddf554001d71f" {
		t.Errorf("Manifest = %+v", m)
	}
	// 第一次触发报 SRV085 → 接受许可协议 → 再触发一次。
	var order []string
	for _, s := range r.cap.reqs {
		if s.method == "POST" && (s.path == dellSA || s.path == dellEULA) {
			order = append(order, filepath.Base(s.path))
			if s.path == dellEULA && string(s.body) != "{}" {
				t.Errorf("SupportAssistAcceptEULA 请求体 = %q", s.body)
			}
		}
	}
	want := []string{"DellLCService.SupportAssistCollection", "DellLCService.SupportAssistAcceptEULA", "DellLCService.SupportAssistCollection"}
	if !slices.Equal(order, want) {
		t.Errorf("调用顺序 = %v，应为 %v", order, want)
	}
	// iDRAC10 完成时返回 204，文件地址在 Location 头。
	if want := []time.Duration{5 * time.Second}; !slices.Equal(r.waits, want) {
		t.Errorf("等待 = %v，应为 %v", r.waits, want)
	}
}

// TestDellEULADeclined：用户没有允许代为接受许可协议时，第一次触发失败就停下来，给出专门的提示。
func TestDellEULADeclined(t *testing.T) {
	c := loadCassette(t, "dell_idrac10_eula.json")
	first := slices.IndexFunc(c.Interactions, func(it *httprec.Interaction) bool {
		return it.Request.Method == "POST" && it.Request.Path == dellSA
	})
	c.Interactions = append(c.Interactions[:first+1], c.Interactions[len(c.Interactions)-1]) // 只留到第一次触发，再加退出会话
	r := newReplayFrom(t, c)
	_, err := Collect(context.Background(), r.req)
	if redfish.KindOf(err) != redfish.KindConsent || redfish.HintOf(err) != dell.EULAHint {
		t.Fatalf("err = %v（%s），应为需要确认并带许可协议提示", err, redfish.KindOf(err))
	}
	if !strings.Contains(err.Error(), "SRV085") {
		t.Errorf("错误里应带设备给的消息：%v", err)
	}
	if len(r.cap.find("POST", dellEULA)) != 0 {
		t.Error("没有允许时不应接受许可协议")
	}
	if len(r.cap.find("DELETE", "/redfish/v1/SessionService/Sessions/31")) != 1 {
		t.Error("失败时也应删除会话")
	}
	if entries, _ := os.ReadDir(filepath.Join(r.dir, "示例客户", "20261026")); len(entries) != 0 {
		t.Errorf("失败时不应留下设备文件夹：%v", entries)
	}
}

// TestIdentifyDellServiceTag：识别回填设备表时，Dell 的序列号也用服务标签。
func TestIdentifyDellServiceTag(t *testing.T) {
	c := loadCassette(t, "dell_idrac9_tsr.json")
	c.Interactions = slices.DeleteFunc(c.Interactions, func(it *httprec.Interaction) bool {
		p := it.Request.Path
		return strings.Contains(p, "/Oem/Dell/") || strings.Contains(p, "/Jobs/") || strings.HasPrefix(p, "/redfish/v1/Dell/")
	})
	p := httprec.NewReplayer(c)
	hc := redfish.NewHTTPClient(redfish.TransportOptions{})
	hc.Transport = p
	id, err := Identify(context.Background(), Request{Address: "bmc.replay.test", User: "admin", Password: "s3cret!", HTTPClient: hc})
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if id.Brand.Key != "dell" || id.Vendor() != "Dell" || id.Serial != "SYNTH01" || id.Firmware != "7.10.70.00" {
		t.Errorf("Identity = %+v", id)
	}
	if left := p.Remaining(); len(left) > 0 {
		t.Errorf("没有退出会话或漏了请求：%v", left)
	}
}

// entriesFile 读附带的 IML.json / IEL.json。
func entriesFile(t *testing.T, path string) (string, []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		ODataID string `json:"@odata.id"`
		Count   int    `json:"Members@odata.count"`
		Members []struct {
			ID      string `json:"Id"`
			Message string
		}
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v.Count != len(v.Members) {
		t.Errorf("%s：Members@odata.count = %d，实际 %d 条", path, v.Count, len(v.Members))
	}
	var ids []string
	for _, m := range v.Members {
		if m.Message == "" {
			t.Errorf("%s：条目 %s 没有内容", path, m.ID)
		}
		ids = append(ids, m.ID)
	}
	return v.ODataID, ids
}

func TestHPEiLO6(t *testing.T) {
	r := newReplay(t, "hpe_ilo6_ahs.json")
	res, err := Collect(context.Background(), r.req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if filepath.Base(res.Dir) != "HPE_ProLiantDL380Gen11_CZJSYNTH01_ahs_20261026-1403" || filepath.Base(res.Path) != "HPE_CZJSYNTH01_20261026.ahs" {
		t.Errorf("Path = %q", res.Path)
	}
	m := res.Manifest
	wantLog := collect.Log{Type: "hpe.ilo.ahs", Method: "redfish-oem",
		Range: collect.Range{From: "2026-10-20T00:00:00+08:00", To: "2026-10-26T23:59:59+08:00"}}
	if m.Log != wantLog || m.Device.Vendor != "HPE" || m.Device.Serial != "CZJSYNTH01" ||
		m.Device.Controller != (collect.Controller{Type: "iLO 6", Firmware: "iLO 6 v1.62"}) {
		t.Errorf("Manifest = %+v", m)
	}
	var roles []string
	for _, f := range m.Files {
		roles = append(roles, f.Name+":"+f.Role)
	}
	if want := []string{"HPE_CZJSYNTH01_20261026.ahs:main", "IML.json:extra", "IEL.json:extra"}; !slices.Equal(roles, want) {
		t.Errorf("Files = %v，应为 %v", roles, want)
	}
	if f := m.Files[0]; f.Size != 130 || f.SHA256 != "b3b4d85edd02a1881496783091d60a6f5758afb5b61a47fb441010665fdbcef7" {
		t.Errorf("AHS = %+v", f)
	}
	// IML 跨两页，合并成一个文件。
	if uri, ids := entriesFile(t, filepath.Join(res.Dir, "IML.json")); uri != "/redfish/v1/Systems/1/LogServices/IML/Entries" || !slices.Equal(ids, []string{"1", "2", "3"}) {
		t.Errorf("IML.json = %s %v", uri, ids)
	}
	if _, ids := entriesFile(t, filepath.Join(res.Dir, "IEL.json")); !slices.Equal(ids, []string{"1", "2"}) {
		t.Errorf("IEL.json = %v", ids)
	}
	onDisk := readManifest(t, res.Dir)
	if rng, _ := onDisk["log"].(map[string]any)["range"].(map[string]any); rng["from"] != "2026-10-20T00:00:00+08:00" {
		t.Errorf("manifest.json 的 log = %v", onDisk["log"])
	}
	// AHS 不需要设备端生成：没有触发和等待。
	if want := []collect.Stage{collect.StageProbe, collect.StageDownload, collect.StageVerify, collect.StageDone}; !slices.Equal(r.stages(), want) {
		t.Errorf("阶段 = %v，应为 %v", r.stages(), want)
	}
	if len(r.waits) != 0 {
		t.Errorf("不应等待：%v", r.waits)
	}
	if len(r.cap.find("DELETE", "/redfish/v1/SessionService/Sessions/admin00000000000a1b2")) != 1 {
		t.Error("结束后应删除会话")
	}
}

// TestHPEiLO5：老固件没有 RecentWeek，按 iLOrest 的办法拼 from/to；IML、IEL 读不到时只保存 AHS。
func TestHPEiLO5(t *testing.T) {
	r := newReplay(t, "hpe_ilo5_ahs.json")
	res, err := Collect(context.Background(), r.req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	m := res.Manifest
	if filepath.Base(res.Path) != "HPE_MXQSYNTH05_20261026.ahs" || len(m.Files) != 1 || m.Files[0].SHA256 != "9b581968d38740ed4b3a88e1b10f370d799b906a7a4031e9e121aa1d91370f18" {
		t.Errorf("Path = %q，Files = %+v", res.Path, m.Files)
	}
	if want := (collect.Range{From: "2026-10-20T00:00:00+08:00", To: "2026-10-25T23:59:59+08:00"}); m.Log.Range != want {
		t.Errorf("Range = %+v，应为 %+v", m.Log.Range, want)
	}
	if m.Device.Controller != (collect.Controller{Type: "iLO 5", Firmware: "iLO 5 v2.30"}) {
		t.Errorf("Controller = %+v", m.Device.Controller)
	}
	for _, id := range []string{"IML", "IEL"} {
		if !slices.ContainsFunc(r.events, func(e collect.Event) bool { return strings.HasPrefix(e.Message, "跳过 "+id+" 日志") }) {
			t.Errorf("应说明跳过了 %s：%v", id, r.events)
		}
	}
}

func TestLenovoXCC(t *testing.T) {
	r := newReplay(t, "lenovo_xcc_ffdc.json")
	res, err := Collect(context.Background(), r.req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if filepath.Base(res.Dir) != "Lenovo_ThinkSystemSR650_J30SYNTH_ffdc_20261026-1403" ||
		filepath.Base(res.Path) != "7X06CTO1WW_J30SYNTH_xcc_20261026-140311.tzz" {
		t.Errorf("Path = %q", res.Path)
	}
	m := res.Manifest
	if m.Log != (collect.Log{Type: "lenovo.xcc.ffdc", Method: "redfish-oem"}) ||
		m.Device.Controller != (collect.Controller{Type: "XCC", Firmware: "TEI3A4P 9.92"}) ||
		m.Files[0].Size != 223 || m.Files[0].SHA256 != "c2af206aff8bc60fdffe7e0b90481a84b48119b4efa0e569f1bb294760d1e49a" {
		t.Errorf("Manifest = %+v", m)
	}
	export := r.cap.find("POST", "/redfish/v1/Managers/1/Oem/Lenovo/ServiceData/Actions/LenovoServiceData.ExportFFDCData")
	if len(export) != 1 || string(export[0].body) != `{"DataCollectionType":"ProcessorDump","InitializationNeeded":true}` {
		t.Errorf("ExportFFDCData 请求 = %q", export)
	}
	// 与官方脚本一样轮询任务资源，完成后的任务里才有文件地址。
	if n := len(r.cap.find("GET", "/redfish/v1/TaskService/Tasks/c7bc86f4-c241-4632-a58a-d83e94e5aecd")); n != 3 {
		t.Errorf("任务资源查了 %d 次，应为 3 次", n)
	}
	if want := []time.Duration{lenovo.PollInterval, lenovo.PollInterval}; !slices.Equal(r.waits, want) {
		t.Errorf("等待 = %v，应为 %v", r.waits, want)
	}
	if !slices.ContainsFunc(r.events, func(e collect.Event) bool { return e.Stage == collect.StageGenerate && e.Percent == 85 }) {
		t.Errorf("没有报告 85%% 的进度：%v", r.events)
	}
}

func TestLenovoXCC3Standard(t *testing.T) {
	r := newReplay(t, "lenovo_xcc3_diag.json")
	res, err := Collect(context.Background(), r.req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	m := res.Manifest
	if filepath.Base(res.Path) != "7DGCCTO1WW_J90SYNTH_xcc_20261026-140311.tzz" ||
		m.Log != (collect.Log{Type: "lenovo.xcc.ffdc", Method: "redfish-standard"}) ||
		m.Files[0].Size != 162 || m.Files[0].SHA256 != "352c61d85e85db39c2422c2003b9dd3ecf78337b78f54a67fce861c633e4e09a" {
		t.Errorf("Path = %q，Manifest = %+v", res.Path, m)
	}
	collectReq := r.cap.find("POST", "/redfish/v1/Systems/1/LogServices/DiagnosticLog/Actions/LogService.CollectDiagnosticData")
	if len(collectReq) != 1 || string(collectReq[0].body) != `{"DiagnosticDataType":"Manager"}` {
		t.Errorf("CollectDiagnosticData 请求 = %q", collectReq)
	}
}
