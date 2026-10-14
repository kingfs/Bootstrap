package app

import (
	"context"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/pkg/hostinfo"
)

// findCheck 按名字取体检项（doctor 的输出顺序是给人看的，测试只认名字）。
func findCheck(checks []Check, name string) (Check, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// TestDoctorTrustStore：系统 CA 证书这条检查必须在"缺证书"时失败并给出可执行命令。
//
// 这条检查来自真实故障：ubuntu:24.04 的 docker 镜像里没有系统证书文件，
// 所有 HTTPS 下载都失败在 x509 校验上，而错误本身没提该装什么。
func TestDoctorTrustStore(t *testing.T) {
	t.Run("缺证书：失败 + 按包管理器给出命令", func(t *testing.T) {
		a, _ := newTestApp(t)
		a.Platform.PackageManagers = []string{"apt-get"}
		a.TrustStore = func() hostinfo.TrustStore {
			return hostinfo.TrustStore{
				Checked:  true,
				Present:  false,
				Searched: []string{"/etc/ssl/certs/ca-certificates.crt"},
			}
		}
		c, ok := findCheck(a.Doctor(context.Background()), "ca certs")
		if !ok {
			t.Fatal("doctor 必须报告 ca certs")
		}
		if c.Status != StatusFail {
			t.Errorf("状态 = %s，期望 fail", c.Status)
		}
		if !strings.Contains(c.Hint, "apt-get install -y ca-certificates") {
			t.Errorf("提示要给出具体命令，实际：%q", c.Hint)
		}
		// 提示里要能看出"找过哪里"，否则用户会怀疑 bs 是不是找错地方了。
		if !strings.Contains(c.Detail, "ca-certificates.crt") {
			t.Errorf("详情应列出找过的路径，实际：%q", c.Detail)
		}
	})

	t.Run("证书齐全：通过并报出路径", func(t *testing.T) {
		a, _ := newTestApp(t)
		a.TrustStore = func() hostinfo.TrustStore {
			return hostinfo.TrustStore{Checked: true, Present: true, Path: "/etc/ssl/certs/ca-certificates.crt"}
		}
		c, ok := findCheck(a.Doctor(context.Background()), "ca certs")
		if !ok {
			t.Fatal("doctor 必须报告 ca certs")
		}
		if c.Status != StatusOK || !strings.Contains(c.Detail, "/etc/ssl/certs") {
			t.Errorf("状态/详情不对：%+v", c)
		}
		if c.Hint != "" {
			t.Errorf("通过时不该给提示：%q", c.Hint)
		}
	})

	t.Run("Windows 不做文件检查：不出这一条", func(t *testing.T) {
		a, _ := newTestApp(t)
		a.TrustStore = func() hostinfo.TrustStore {
			return hostinfo.TrustStore{Checked: false, Present: true}
		}
		if _, ok := findCheck(a.Doctor(context.Background()), "ca certs"); ok {
			t.Error("Checked=false 时不该报告这项（Windows 走系统证书存储）")
		}
	})
}

// TestTrustStoreHintFallback：认不出包管理器时也得给出可行动的提示，而不是空话。
func TestTrustStoreHintFallback(t *testing.T) {
	hint := trustStoreHint(hostinfo.Platform{PackageManagers: []string{"brew"}})
	if !strings.Contains(hint, "ca-certificates") {
		t.Errorf("提示应提到 ca-certificates 包名：%q", hint)
	}
	if strings.Contains(hint, "apt-get") {
		t.Errorf("没有 apt 时不该出现 apt 命令：%q", hint)
	}
}
