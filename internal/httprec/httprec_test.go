// SPDX-License-Identifier: GPL-3.0-or-later

package httprec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testPassword = "s3cret-pass"
	testToken    = "tok-0123456789abcdef"
	testCSRF     = "csrf-abcdef"
)

// fakeDevice 模拟一台设备：登录返回令牌和绝对地址 Location，另有 JSON 资源和一个大文件。
func fakeDevice(t *testing.T, big []byte) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Auth-Token", testToken)
		w.Header().Set("Location", srv.URL+"/sessions/1")
		http.SetCookie(w, &http.Cookie{Name: "QSESSIONID", Value: "cookie-value"})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"CSRFToken": testCSRF, "ok": true})
	})
	mux.HandleFunc("GET /data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		host := strings.Split(r.Host, ":")[0]
		io.WriteString(w, `{"Name":"BMC","PasswordChangeRequired":false,"Address":"`+host+`","Other":"`+host+`0"}`)
	})
	mux.HandleFunc("GET /file", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(big)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type flow struct {
	location string
	data     map[string]any
	fileSum  string
}

// runFlow 依次登录、读资源、下载文件，返回关键结果供录制和回放对比。
func runFlow(t *testing.T, base string, rt http.RoundTripper) flow {
	t.Helper()
	c := &http.Client{Transport: rt}
	var f flow

	req, _ := http.NewRequest("POST", base+"/login?token="+testToken, strings.NewReader(`{"UserName":"admin","Password":"`+testPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+testPassword)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	f.location = resp.Header.Get("Location")

	req, _ = http.NewRequest("GET", base+"/data", nil)
	req.Header.Set("X-Auth-Token", testToken)
	resp, err = c.Do(req)
	if err != nil {
		t.Fatalf("data: %v", err)
	}
	json.NewDecoder(resp.Body).Decode(&f.data)
	resp.Body.Close()

	resp, err = c.Get(base + "/file")
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	h := sha256.New()
	io.Copy(h, resp.Body)
	resp.Body.Close()
	f.fileSum = hex.EncodeToString(h.Sum(nil))
	return f
}

func TestRecordThenReplay(t *testing.T) {
	big := bytes.Repeat([]byte("LOGDATA-"), 64<<10) // 512 KiB，超过录制上限
	srv := fakeDevice(t, big)

	rec := NewRecorder(http.DefaultTransport)
	live := runFlow(t, srv.URL, rec)
	if live.location != srv.URL+"/sessions/1" {
		t.Fatalf("live location = %q", live.location)
	}

	path := filepath.Join(t.TempDir(), "rec.json")
	if err := rec.Cassette().Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := readFile(path)
	host := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[0]
	for _, secret := range []string{testPassword, testToken, testCSRF, "cookie-value", srv.URL, `"` + host + `"`} {
		if strings.Contains(raw, secret) {
			t.Errorf("录制文件里不应出现 %q", secret)
		}
	}
	for _, want := range []string{BasePlaceholder + "/sessions/1", `"PasswordChangeRequired": false`, host + "0", `"truncated": true`} {
		if !strings.Contains(raw, want) {
			t.Errorf("录制文件里应有 %q", want)
		}
	}

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	fileBody := c.Interactions[2].Response.Body
	if fileBody.Size != int64(len(big)) || fileBody.SHA256 != live.fileSum || len(fileBody.Text) != previewBytes {
		t.Errorf("大文件应只保存开头 %d 字节并记下大小和校验值，实际 size=%d sha=%s len=%d", previewBytes, fileBody.Size, fileBody.SHA256, len(fileBody.Text))
	}

	p := NewReplayer(c)
	replayed := runFlow(t, "https://bmc.replay.test", p)
	if replayed.location != "https://bmc.replay.test/sessions/1" {
		t.Errorf("replayed location = %q", replayed.location)
	}
	if replayed.data["Address"] != "bmc.replay.test" || replayed.data["Name"] != "BMC" {
		t.Errorf("replayed data = %v", replayed.data)
	}
	if left := p.Remaining(); len(left) != 0 {
		t.Errorf("还有未回放的条目：%v", left)
	}
}

func TestReplayerSequence(t *testing.T) {
	c := &Cassette{Format: Format, Interactions: []*Interaction{
		{Request: Request{Method: "GET", Path: "/task"}, Response: Response{Status: 202, Body: Body{JSON: json.RawMessage(`{"n":1}`)}}},
		{Request: Request{Method: "GET", Path: "/task"}, Response: Response{Status: 200, Body: Body{JSON: json.RawMessage(`{"n":2}`)}}},
		{Request: Request{Method: "GET", Path: "/down"}, Error: "dial tcp: connection refused"},
	}}
	p := NewReplayer(c)
	client := &http.Client{Transport: p}
	for i, want := range []int{202, 200} {
		resp, err := client.Get("http://x/task")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("第 %d 次：状态码 %d，应为 %d", i+1, resp.StatusCode, want)
		}
	}
	if _, err := client.Get("http://x/task"); err == nil {
		t.Error("录制条目用完后应报错")
	}
	if _, err := client.Get("http://x/down"); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("应回放录制时的网络错误，实际 %v", err)
	}
	if got := p.Missed(); len(got) != 1 || got[0] != "GET /task" {
		t.Errorf("Missed = %v", got)
	}
}

func TestRedactJSON(t *testing.T) {
	in := `{"UserName":"admin","Password":"x","PasswordChangeRequired":true,"Oem":{"Token":"y","List":[{"passwd":"z","Name":"keep"}]}}`
	out := string(redactJSON([]byte(in)))
	for _, s := range []string{`"x"`, `"y"`, `"z"`} {
		if strings.Contains(out, s) {
			t.Errorf("没有去掉 %s：%s", s, out)
		}
	}
	for _, s := range []string{`"admin"`, `"keep"`, `"PasswordChangeRequired":true`} {
		if !strings.Contains(out, s) {
			t.Errorf("误删了 %s：%s", s, out)
		}
	}
	if same := redactJSON([]byte(`{"a":1}`)); string(same) != `{"a":1}` {
		t.Errorf("没有凭据时应原样保留，实际 %s", same)
	}
}

func TestRedactForm(t *testing.T) {
	out := string(redactForm([]byte("username=admin&password=hunter2")))
	if strings.Contains(out, "hunter2") || !strings.Contains(out, "username=admin") {
		t.Errorf("redactForm = %s", out)
	}
}

func TestReplaceHost(t *testing.T) {
	got := replaceHost("10.0.0.1, 10.0.0.10, a10.0.0.1, https://10.0.0.1/x", "10.0.0.1", HostPlaceholder)
	want := "{{host}}, 10.0.0.10, a10.0.0.1, https://{{host}}/x"
	if got != want {
		t.Errorf("replaceHost = %q，应为 %q", got, want)
	}
}

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}
