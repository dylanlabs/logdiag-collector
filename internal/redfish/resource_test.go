// SPDX-License-Identifier: GPL-3.0-or-later

package redfish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestParseActions(t *testing.T) {
	raw := json.RawMessage(`{
		"#Manager.Reset": {"target": "/redfish/v1/Managers/1/Actions/Manager.Reset", "ResetType@Redfish.AllowableValues": ["ForceRestart"]},
		"Oem": {"xFusion": {
			"#Manager.Dump": {"target": "/redfish/v1/Managers/1/Actions/Oem/xFusion/Manager.Dump", "Type@Redfish.AllowableValues": ["URI"]},
			"#Manager.GeneralDownload": {"target": "/redfish/v1/Managers/1/Actions/Oem/xFusion/Manager.GeneralDownload"}
		}}
	}`)
	actions := ParseActions(raw)
	if len(actions) != 3 {
		t.Fatalf("actions = %+v", actions)
	}
	dump, ok := FindAction(actions, "Manager.Dump")
	if !ok || dump.Vendor != "xFusion" || dump.Target != "/redfish/v1/Managers/1/Actions/Oem/xFusion/Manager.Dump" ||
		!slices.Equal(dump.Allowable["Type"], []string{"URI"}) {
		t.Errorf("Manager.Dump = %+v, %v", dump, ok)
	}
	if reset, ok := FindAction(actions, "#Manager.Reset"); !ok || reset.Vendor != "" {
		t.Errorf("Manager.Reset = %+v, %v", reset, ok)
	}
	if _, ok := FindAction(actions, "LogService.CollectDiagnosticData"); ok {
		t.Error("不存在的动作不应找到")
	}
	if ParseActions(nil) != nil {
		t.Error("没有 Actions 时应返回 nil")
	}
}

// 华为 iBMC 的任务消息是单个对象，进度放在 Oem.Huawei.TaskPercentage。
func TestTaskVendorFormats(t *testing.T) {
	cases := []struct {
		body    string
		percent int
		text    string
	}{
		{`{"TaskState":"Running","Messages":[],"Oem":{"Huawei":{"TaskPercentage":null}}}`, -1, ""},
		{`{"TaskState":"Running","Messages":[],"Oem":{"Huawei":{"TaskPercentage":"40%"}}}`, 40, ""},
		{`{"TaskState":"Completed","Messages":{"MessageId":"Base.1.0.Success","Message":"Done"},"Oem":{"xFusion":{"TaskPercentage":"100%"}}}`, 100, "Done"},
		{`{"TaskState":"Running","PercentComplete":10,"Oem":{"Huawei":{"TaskPercentage":"90%"}}}`, 10, ""},
		{`{"TaskState":"Exception","Messages":[{"Message":"bad","MessageArgs":["a",3,true]}]}`, -1, "bad"},
	}
	for _, tc := range cases {
		task := parseTask(&Response{Body: []byte(tc.body)})
		if task == nil {
			t.Errorf("%s：应能解析为任务", tc.body)
			continue
		}
		if task.Percent() != tc.percent || task.Text() != tc.text {
			t.Errorf("%s：Percent = %d, Text = %q", tc.body, task.Percent(), task.Text())
		}
	}
	task := parseTask(&Response{Body: []byte(cases[4].body)})
	if got := task.Messages[0].Args; !slices.Equal(got, MessageArgs{"a", "3", "true"}) {
		t.Errorf("MessageArgs = %q", got)
	}
}

func TestDownloadAction(t *testing.T) {
	var gotBody, gotToken, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotToken, gotMethod = string(b), r.Header.Get("X-Auth-Token"), r.Method
		if r.URL.Path != "/redfish/v1/Managers/1/Actions/Oem/Huawei/Manager.GeneralDownload" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"Base.1.0.GeneralError","message":"not found"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "file-content")
	}))
	defer srv.Close()
	c, err := New(srv.URL, Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	c.token = "tok"

	var buf bytes.Buffer
	n, sum, err := c.DownloadAction(context.Background(), "/redfish/v1/Managers/1/Actions/Oem/Huawei/Manager.GeneralDownload",
		map[string]string{"TransferProtocol": "HTTPS", "Path": "/tmp/web/dump_info.tar.gz"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotToken != "tok" || gotBody != `{"Path":"/tmp/web/dump_info.tar.gz","TransferProtocol":"HTTPS"}` {
		t.Errorf("请求 = %s %q %q", gotMethod, gotToken, gotBody)
	}
	if buf.String() != "file-content" || n != 12 || sum != "2239ce4df9ee8db012834642ec801b55ba2c92b28bdd11f4d73d9c55d39f3b0a" {
		t.Errorf("下载 = %q, %d, %s", buf.String(), n, sum)
	}

	_, _, err = c.DownloadAction(context.Background(), "/redfish/v1/missing", nil, io.Discard)
	if KindOf(err) != KindUnsupported {
		t.Errorf("404 应归为接口不支持：%v", err)
	}
}

func TestKindOfUnsupported(t *testing.T) {
	err := fmt.Errorf("没有一键收集动作：%w", ErrUnsupported)
	if KindOf(err) != KindUnsupported {
		t.Errorf("KindOf = %s", KindOf(err))
	}
}
