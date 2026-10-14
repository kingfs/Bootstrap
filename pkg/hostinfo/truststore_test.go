package hostinfo

import (
	"errors"
	"os"
	"testing"
	"time"
)

// fakeStat 只认识给定的文件集合，其余一律 ErrNotExist（探测必须只看"存在与否"）。
func fakeStat(present ...string) func(string) (os.FileInfo, error) {
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	return func(name string) (os.FileInfo, error) {
		if set[name] {
			return fakeFileInfo{name: name}, nil
		}
		return nil, errors.New("not exist")
	}
}

type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

func noEnv(string) string { return "" }

func TestTrustStoreFiles(t *testing.T) {
	linux := TrustStoreFiles("linux")
	if len(linux) == 0 {
		t.Fatal("linux 必须有候选路径")
	}
	// Debian/Ubuntu 的路径必须在列：这是本仓库的主力平台。
	found := false
	for _, p := range linux {
		if p == "/etc/ssl/certs/ca-certificates.crt" {
			found = true
		}
	}
	if !found {
		t.Errorf("linux 候选应当包含 Debian/Ubuntu 的 ca-certificates.crt：%v", linux)
	}
	if got := TrustStoreFiles("darwin"); len(got) != 1 || got[0] != "/etc/ssl/cert.pem" {
		t.Errorf("darwin 候选 = %v", got)
	}
	if got := TrustStoreFiles("windows"); got != nil {
		t.Errorf("windows 走系统证书存储，不该有文件候选：%v", got)
	}
}

func TestCheckTrustStore(t *testing.T) {
	debian := "/etc/ssl/certs/ca-certificates.crt"

	t.Run("没有任何证书文件", func(t *testing.T) {
		ts := CheckTrustStore("linux", noEnv, fakeStat())
		if ts.Present {
			t.Error("不该报 present")
		}
		if !ts.Checked {
			t.Error("linux 必须做检查")
		}
		// 提示里要能看到"找过哪些地方"，否则用户不知道该装哪个包。
		if len(ts.Searched) != len(TrustStoreFiles("linux")) {
			t.Errorf("Searched 应覆盖全部候选：%v", ts.Searched)
		}
	})

	t.Run("命中 Debian/Ubuntu 路径", func(t *testing.T) {
		ts := CheckTrustStore("linux", noEnv, fakeStat(debian))
		if !ts.Present || ts.Path != debian {
			t.Errorf("应当命中 %s：%+v", debian, ts)
		}
	})

	t.Run("RHEL 家族只有 ca-bundle 也算有", func(t *testing.T) {
		ts := CheckTrustStore("linux", noEnv, fakeStat("/etc/pki/tls/certs/ca-bundle.crt"))
		if !ts.Present || ts.Path != "/etc/pki/tls/certs/ca-bundle.crt" {
			t.Errorf("应当命中 RHEL 路径：%+v", ts)
		}
	})

	t.Run("SSL_CERT_FILE 优先（Go 认它）", func(t *testing.T) {
		env := func(k string) string {
			if k == "SSL_CERT_FILE" {
				return "/opt/certs/corp.pem"
			}
			return ""
		}
		ts := CheckTrustStore("linux", env, fakeStat("/opt/certs/corp.pem", debian))
		if !ts.Present || ts.Path != "/opt/certs/corp.pem" {
			t.Errorf("应当优先认 SSL_CERT_FILE：%+v", ts)
		}
	})

	t.Run("SSL_CERT_FILE 指向不存在的文件时继续找默认路径", func(t *testing.T) {
		env := func(k string) string {
			if k == "SSL_CERT_FILE" {
				return "/nope/missing.pem"
			}
			return ""
		}
		ts := CheckTrustStore("linux", env, fakeStat(debian))
		if !ts.Present || ts.Path != debian {
			t.Errorf("应当退回默认路径：%+v", ts)
		}
	})

	t.Run("SSL_CERT_DIR 指向目录也算有", func(t *testing.T) {
		env := func(k string) string {
			if k == "SSL_CERT_DIR" {
				return "/etc/ssl/certs"
			}
			return ""
		}
		stat := func(name string) (os.FileInfo, error) {
			if name == "/etc/ssl/certs" {
				return fakeFileInfo{name: name, dir: true}, nil
			}
			return nil, errors.New("not exist")
		}
		ts := CheckTrustStore("linux", env, stat)
		if !ts.Present || ts.Path != "/etc/ssl/certs" {
			t.Errorf("应当认 SSL_CERT_DIR：%+v", ts)
		}
	})

	t.Run("目录不算证书文件", func(t *testing.T) {
		stat := func(string) (os.FileInfo, error) { return fakeFileInfo{dir: true}, nil }
		if ts := CheckTrustStore("linux", noEnv, stat); ts.Present {
			t.Errorf("目录不该判为 present：%+v", ts)
		}
	})

	t.Run("windows 不做文件检查", func(t *testing.T) {
		ts := CheckTrustStore("windows", noEnv, fakeStat())
		if ts.Checked {
			t.Error("windows 不该做文件检查")
		}
		if !ts.Present {
			t.Error("windows 走系统证书存储，应视为可用")
		}
	})
}
