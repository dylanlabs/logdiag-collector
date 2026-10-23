// SPDX-License-Identifier: GPL-3.0-or-later

package redfish

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/httprec"
)

// replayClient 用 testdata 里的录制记录代替设备。轮询等待被记录下来而不真的等待。
func replayClient(t *testing.T, name string) (*Client, *httprec.Replayer, *[]time.Duration) {
	t.Helper()
	p, err := httprec.LoadReplayer(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	c, err := New("bmc.replay.test", Options{HTTPClient: &http.Client{Transport: p}})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() {
		if left := p.Remaining(); len(left) > 0 {
			t.Errorf("录制记录里还有没走到的请求：%v", left)
		}
	})
	return c, p, &waits
}

func TestReplaySession(t *testing.T) {
	c, _, _ := replayClient(t, "session.json")
	ctx := context.Background()

	if err := c.Login(ctx, "admin", "secret"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !c.LoggedIn() {
		t.Fatal("登录后应持有会话")
	}
	// 绝对地址的 Location 只保留路径。
	if c.sessionURI != "/redfish/v1/SessionService/Sessions/1a2b3c" {
		t.Errorf("sessionURI = %q", c.sessionURI)
	}
	var coll struct {
		Members []ODataID
	}
	if _, err := c.Get(ctx, "/redfish/v1/Managers", &coll); err != nil {
		t.Fatal(err)
	}
	if len(coll.Members) != 1 || coll.Members[0].ID != "/redfish/v1/Managers/1" {
		t.Errorf("Members = %v", coll.Members)
	}
	if err := c.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if c.LoggedIn() {
		t.Error("退出后不应再持有会话")
	}
}

func TestReplaySessionUnauthorized(t *testing.T) {
	c, _, _ := replayClient(t, "session_unauthorized.json")
	err := c.Login(context.Background(), "admin", "wrong")
	if KindOf(err) != KindAuth {
		t.Fatalf("KindOf(%v) = %v，应为认证失败", err, KindOf(err))
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.MessageID != "Base.1.16.0.ResourceAtUriUnauthorized" {
		t.Errorf("应带出设备的 MessageId，实际 %v", err)
	}
}

func TestReplayTaskMonitor(t *testing.T) {
	c, _, waits := replayClient(t, "task_monitor.json")
	ctx := context.Background()
	var progress []Progress

	res, err := c.RunAction(ctx, "/redfish/v1/Systems/1/LogServices/DiagnosticLog/Actions/LogService.CollectDiagnosticData",
		map[string]string{"DiagnosticDataType": "Manager"},
		PollOptions{OnProgress: func(p Progress) { progress = append(progress, p) }})
	if err != nil {
		t.Fatalf("RunAction: %v", err)
	}
	if res.Task != nil || res.Response.StatusCode != http.StatusCreated {
		t.Errorf("任务监视器直接返回操作结果时，Task 应为 nil、状态码 201，实际 %+v", res)
	}
	entryURI := res.Location()
	if entryURI != "/redfish/v1/Systems/1/LogServices/DiagnosticLog/Entries/5" {
		t.Fatalf("Location = %q", entryURI)
	}
	if want := []Progress{{TaskRunning, 10, ""}, {TaskRunning, 60, ""}}; !slices.Equal(progress, want) {
		t.Errorf("progress = %v，应为 %v", progress, want)
	}
	// Retry-After: 10 照办；Retry-After: 120 被限制在 MaxInterval（30 秒）。
	if want := []time.Duration{10 * time.Second, 30 * time.Second}; !slices.Equal(*waits, want) {
		t.Errorf("waits = %v，应为 %v", *waits, want)
	}

	var entry struct {
		AdditionalDataURI       string
		AdditionalDataSizeBytes int64
	}
	if _, err := c.Get(ctx, entryURI, &entry); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n, sum, err := c.Download(ctx, entry.AdditionalDataURI, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != entry.AdditionalDataSizeBytes || sum != "7c7c33f381b4a23e0997a812bbdd73467a8600a0f12420f203c1ecca0f1a981b" {
		t.Errorf("下载 %d 字节，SHA-256 %s", n, sum)
	}
}

func TestReplayTaskResource(t *testing.T) {
	c, _, _ := replayClient(t, "task_resource.json")
	var progress []Progress
	res, err := c.RunAction(context.Background(), "/redfish/v1/Managers/1/Actions/Oem/Contoso/Manager.Dump", nil,
		PollOptions{OnProgress: func(p Progress) { progress = append(progress, p) }})
	if err != nil {
		t.Fatalf("RunAction: %v", err)
	}
	if res.Task == nil || res.Task.State() != TaskCompleted {
		t.Fatalf("应以 Completed 结束，实际 %+v", res.Task)
	}
	if args := res.Task.Messages[0].Args; len(args) != 1 || args[0] != "/tmp/web/dump_info.tar.gz" {
		t.Errorf("完成消息里的文件路径 = %v", args)
	}
	want := []Progress{{TaskRunning, 20, "Collecting files."}, {TaskRunning, 80, "Compressing files."}}
	if !slices.Equal(progress, want) {
		t.Errorf("progress = %v，应为 %v", progress, want)
	}
}

func TestReplayTaskJob(t *testing.T) {
	c, _, _ := replayClient(t, "task_job.json")
	ctx := context.Background()
	start, err := c.StartAction(ctx,
		"/redfish/v1/Managers/iDRAC.Embedded.1/Oem/Dell/DellLCService/Actions/DellLCService.SupportAssistCollection",
		map[string]any{"ShareType": "Local", "DataSelectorArrayIn": []string{"HWData", "TTYLogs"}})
	if err != nil {
		t.Fatal(err)
	}
	if start.Monitor != "/redfish/v1/Managers/iDRAC.Embedded.1/Jobs/JID_612345678901" {
		t.Fatalf("Monitor = %q", start.Monitor)
	}
	// Dell 驱动改为轮询 TaskService/Tasks/<JobID>，完成时的 Location 才指向文件。
	jobID := filepath.Base(start.Monitor)
	res, err := c.WaitTask(ctx, "/redfish/v1/TaskService/Tasks/"+jobID, PollOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if loc := res.Location(); loc != "/redfish/v1/Dell/sacollect.zip" {
		t.Fatalf("Location = %q", loc)
	}
	var buf bytes.Buffer
	if _, sum, err := c.Download(ctx, res.Location(), &buf); err != nil || sum != "85203f3001bc70c436f89ed016388aded789cbca01af374887dd662e6db4b090" {
		t.Errorf("Download: sum=%s err=%v", sum, err)
	}
}

func TestReplayTaskException(t *testing.T) {
	c, _, _ := replayClient(t, "task_exception.json")
	res, err := c.RunAction(context.Background(), "/redfish/v1/Systems/1/LogServices/DiagnosticLog/Actions/LogService.CollectDiagnosticData",
		map[string]string{"DiagnosticDataType": "Manager"}, PollOptions{})
	var te *TaskError
	if !errors.As(err, &te) || te.State != TaskException || len(te.Messages) != 1 {
		t.Fatalf("应返回 Exception 的 TaskError，实际 %v", err)
	}
	if KindOf(err) != KindTaskFailed {
		t.Errorf("KindOf = %v", KindOf(err))
	}
	if res == nil || res.Task == nil || res.Task.TaskStatus != "Critical" {
		t.Errorf("失败时也应返回最后的任务资源，实际 %+v", res)
	}
}
