// SPDX-License-Identifier: GPL-3.0-or-later

package oscheck_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/oscheck"
	"github.com/dylanlabs/logdiag-collector/internal/sshx"
	"github.com/dylanlabs/logdiag-collector/internal/sshx/sshtest"
)

// fakeBin 在临时目录里放几个假命令，让流程脱离真机也能跑：uname 永远报 Linux，id 和 sudo
// 由环境变量控制。其余命令（bash、tar、cksum、mktemp、tee、cat、rm、find）用系统真命令。
func fakeBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("uname", "#!/bin/sh\necho Linux\n")
	write("hostname", "#!/bin/sh\necho testhost\n")
	write("id", "#!/bin/sh\necho \"${FAKE_UID:-1000}\"\n")
	// 假 sudo：解析 -n/-S/-p/-k，按环境变量判断免密或校验密码，再执行其余命令。
	write("sudo", `#!/bin/sh
stdin=0; nopass=0
while [ $# -gt 0 ]; do
  case "$1" in
    -n) nopass=1; shift ;;
    -S) stdin=1; shift ;;
    -k) shift ;;
    -p) shift; shift ;;
    --) shift; break ;;
    -*) shift ;;
    *) break ;;
  esac
done
if [ "$nopass" = 1 ]; then
  [ "${FAKE_SUDO_NOPASS:-0}" = 1 ] || { echo "sudo: a password is required" >&2; exit 1; }
elif [ "$stdin" = 1 ]; then
  IFS= read -r pw
  [ "$pw" = "$FAKE_SUDO_PASS" ] || { echo "sudo: incorrect password" >&2; exit 1; }
fi
exec "$@"
`)
	return dir
}

// fakeScript 返回一个小巡检脚本：打印进度行和一条 WARNING，把结果写到 outDir 下，
// 打印和真脚本一样的结果路径行，退出 0。
func fakeScript(outDir string) oscheck.Script {
	name := "testhost.ABC123.pm-20261024_000000"
	archive := filepath.Join(outDir, name+".tar.gz")
	body := fmt.Sprintf(`#!/bin/bash
echo "Collecting system information ..."
echo "Collecting hardware information ..."
echo "WARNING: not running as root, some hardware/log data will be incomplete."
d=%q
mkdir -p "$d"
echo hello > "$d/info.txt"
cd %q || exit 1
tar -czf %q "%s" && rm -rf "$d"
echo "Inspection data saved to %s"
exit 0
`, filepath.Join(outDir, name), outDir, name+".tar.gz", name, archive)
	return oscheck.Script{
		OS: "linux", Name: "fake-pm.sh", Title: "Linux", Shell: "bash", LogType: "logdiag.linux.pm",
		OutDir: outDir, OutPattern: "*.pm-*.tar.gz", Content: []byte(body),
	}
}

func TestInspect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用本机 sh 执行命令，只在类 Unix 上跑")
	}
	for _, tc := range []struct {
		name                     string
		uid, sudoNopass, elevate string
		wantPrivWarn             bool
	}{
		{name: "root", uid: "0"},
		{name: "sudo-password", uid: "1000", elevate: "1"},
		{name: "sudo-nopass", uid: "1000", sudoNopass: "1", elevate: "1"},
		{name: "plain-no-elevate", uid: "1000", wantPrivWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeBin(t)
			outDir := t.TempDir()
			// 登录密码和 sudo 密码是同一个 OS 账号密码。
			const pass = "ops-pass"
			env := []string{
				"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"FAKE_UID=" + tc.uid,
				"FAKE_SUDO_NOPASS=" + tc.sudoNopass,
				"FAKE_SUDO_PASS=" + pass,
			}
			s := sshtest.New(t, sshtest.Options{User: "ops", Password: pass},
				sshtest.LocalShell(t.TempDir(), env...))

			rec := sshx.NewRecorder()
			res, err := oscheck.Inspect(context.Background(), oscheck.Request{
				Address:  s.Addr,
				User:     "ops",
				Password: pass,
				HostKey:  s.HostKey,
				Elevate:  tc.elevate == "1",
				Device:   collect.Device{Type: "os", Vendor: "TestVendor", Model: "T100", Serial: "ABC123"},
				Store:    &collect.Store{Dir: t.TempDir(), Tool: collect.Tool{Name: "LogDiag Collector", Version: "test"}},
				Recorder: rec,
			}, fakeScript(outDir))
			if err != nil {
				t.Fatalf("Inspect 失败：%v", err)
			}

			// 结果文件存在、manifest 正确。
			if _, err := os.Stat(res.Path); err != nil {
				t.Errorf("结果文件不存在：%v", err)
			}
			m := res.Manifest
			if m.Device.Type != "os" || m.Log.Type != "logdiag.linux.pm" || m.Log.Method != "ssh" {
				t.Errorf("manifest 不对：type=%s log=%+v", m.Device.Type, m.Log)
			}
			if m.Device.Serial != "ABC123" || m.Device.Hostname != "testhost" {
				t.Errorf("设备信息不对：%+v", m.Device)
			}
			if res.HostKey == "" || !strings.Contains(res.HostKey, s.HostKey) {
				t.Errorf("没有返回主机密钥：%q", res.HostKey)
			}

			// 脚本的 WARNING 转成了告警。
			if !hasContains(res.Warnings, "not running as root") {
				t.Errorf("没有转发脚本告警：%v", res.Warnings)
			}
			if tc.wantPrivWarn && !hasContains(res.Warnings, "sudo") {
				t.Errorf("普通账号应提示没有提权：%v", res.Warnings)
			}

			// 临时文件清理干净：outDir 下不应留结果文件或工作目录，/tmp 里不应留上传目录。
			left, _ := filepath.Glob(filepath.Join(outDir, "*"))
			if len(left) != 0 {
				t.Errorf("结果目录没有清理干净：%v", left)
			}
			stages, _ := filepath.Glob("/tmp/logdiag-inspect.*")
			if len(stages) != 0 {
				t.Errorf("上传目录没有清理干净：%v", stages)
			}

			// 诊断记录里不含密码。
			r := rec.Record()
			if dump := recordString(r); strings.Contains(dump, "ops-pass") {
				t.Error("诊断记录里出现了密码")
			}
		})
	}
}

// TestInspectWrongOS：系统不是脚本针对的系统时报错，不执行脚本。
func TestInspectWrongOS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用本机 sh 执行命令")
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "uname"), []byte("#!/bin/sh\necho SunOS\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 0\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "hostname"), []byte("#!/bin/sh\necho h\n"), 0o755)
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	s := sshtest.New(t, sshtest.Options{User: "ops", Password: "p"}, sshtest.LocalShell(t.TempDir(), env...))
	_, err := oscheck.Inspect(context.Background(), oscheck.Request{
		Address: s.Addr, User: "ops", Password: "p", HostKey: s.HostKey,
		Store: &collect.Store{Dir: t.TempDir()},
	}, fakeScript(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "SunOS") {
		t.Fatalf("错误 = %v，应说明系统不符", err)
	}
}

// TestInspectCancelCleansUp：执行中取消，设备上的临时文件仍要清理。
func TestInspectCancelCleansUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用本机 sh 执行命令")
	}
	bin := fakeBin(t)
	outDir := t.TempDir()
	// 这个脚本会卡住（sleep），等被取消；工作目录已经建好但不会压缩。
	name := "testhost.ZZZ.pm-20261024_000001"
	body := fmt.Sprintf("#!/bin/bash\nmkdir -p %q\necho x > %q\necho 'Collecting ...'\nsleep 60\n",
		filepath.Join(outDir, name), filepath.Join(outDir, name, "f"))
	script := oscheck.Script{OS: "linux", Name: "slow.sh", Title: "Linux", LogType: "logdiag.linux.pm",
		OutDir: outDir, OutPattern: "*.pm-*", Content: []byte(body)}
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "FAKE_UID=0"}
	s := sshtest.New(t, sshtest.Options{User: "ops", Password: "p"}, sshtest.LocalShell(t.TempDir(), env...))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	_, err := oscheck.Inspect(ctx, oscheck.Request{
		Address: s.Addr, User: "ops", Password: "p", HostKey: s.HostKey,
		Store: &collect.Store{Dir: t.TempDir()},
	}, script)
	if err == nil {
		t.Fatal("取消后应返回错误")
	}
	// 被取消也要清理掉残留的工作目录和上传目录。
	if left, _ := filepath.Glob(filepath.Join(outDir, "*")); len(left) != 0 {
		t.Errorf("取消后残留工作目录没有清理：%v", left)
	}
	if stages, _ := filepath.Glob("/tmp/logdiag-inspect.*"); len(stages) != 0 {
		t.Errorf("取消后上传目录没有清理：%v", stages)
	}
}

func hasContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func recordString(r *sshx.Record) string {
	var b strings.Builder
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s\n", s.Kind, s.Command, s.Text, s.Stdout.Text, s.Stderr.Text)
	}
	return b.String()
}
