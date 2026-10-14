package hostinfo

import (
	"os"
	"runtime"
)

// TrustStore 描述"系统 CA 证书"的可用性。
//
// 为什么需要它：全新最小镜像（例如 ubuntu:24.04 的 docker 镜像）里**没有**
// /etc/ssl/certs/ca-certificates.crt，于是第一次 HTTPS 请求就以
// `x509: certificate signed by unknown authority` 失败——而这句话完全没告诉
// 使用者"该装 CA 证书"。这里把"有没有、在哪、找过哪些路径"变成可判断的事实，
// 好让 bs doctor 直接给出那一步。
type TrustStore struct {
	// Present 表示证书可用（找到文件，或由环境变量指定且存在）。
	Present bool `json:"present"`
	// Path 是命中的位置（Present 为真时有意义）。
	Path string `json:"path,omitempty"`
	// Searched 是检查过的路径，用于提示"我找过这些地方都没有"。
	Searched []string `json:"searched,omitempty"`
	// Checked 为假表示该平台不做文件检查（Windows 走系统证书存储）。
	Checked bool `json:"checked"`
}

// TrustStoreFiles 返回该平台上 Go 的 TLS 校验会读取的系统证书文件候选（按优先级）。
//
// 不是"随手列几个路径"：Go 的 crypto/x509 在 Linux 上读的就是这几处，对应各发行版
// 打包 CA 证书的位置——Debian/Ubuntu 的 ca-certificates.crt、RHEL 家族的
// ca-bundle.crt、Arch/SUSE 的 ca-bundle.crt、Alpine 的 cert.pem；macOS 是
// /etc/ssl/cert.pem。Windows 用系统证书存储（没有可查的文件），返回 nil。
func TrustStoreFiles(goos string) []string {
	switch goos {
	case "linux":
		return []string{
			"/etc/ssl/certs/ca-certificates.crt", // Debian / Ubuntu / Arch / Alpine
			"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL / Fedora / CentOS
			"/etc/ssl/certs/ca-bundle.crt",       // SUSE / 部分发行版
			"/etc/ssl/cert.pem",                  // Alpine 旧版 / 部分镜像
		}
	case "darwin":
		return []string{"/etc/ssl/cert.pem"}
	default:
		return nil
	}
}

// CheckTrustStore 用注入的 stat 检查系统证书；goos 决定候选路径。
//
// 先看 SSL_CERT_FILE / SSL_CERT_DIR（Go 认这两个环境变量），再看发行版默认路径——
// 顺序与 Go 自己一致，否则会出现"bs 说没有、Go 却连得上"的假警报。
func CheckTrustStore(goos string, getenv func(string) string, stat func(string) (os.FileInfo, error)) TrustStore {
	ts := TrustStore{Checked: true}
	if goos == "windows" {
		ts.Checked = false
		ts.Present = true // 走系统证书存储，不做文件检查
		return ts
	}
	if getenv != nil {
		// 只认"文件存在且不是目录"——Go 用的是文件；目录形式由 SSL_CERT_DIR 表达。
		if f := getenv("SSL_CERT_FILE"); f != "" {
			ts.Searched = append(ts.Searched, f+" (SSL_CERT_FILE)")
			if okFile(stat, f) {
				ts.Present, ts.Path = true, f
				return ts
			}
		}
		if d := getenv("SSL_CERT_DIR"); d != "" {
			ts.Searched = append(ts.Searched, d+" (SSL_CERT_DIR)")
			if info, err := stat(d); err == nil && info.IsDir() {
				ts.Present, ts.Path = true, d
				return ts
			}
		}
	}
	for _, p := range TrustStoreFiles(goos) {
		ts.Searched = append(ts.Searched, p)
		if okFile(stat, p) {
			ts.Present, ts.Path = true, p
			return ts
		}
	}
	return ts
}

// DetectTrustStore 用真实环境探测。
func DetectTrustStore() TrustStore {
	return CheckTrustStore(runtime.GOOS, os.Getenv, os.Stat)
}

func okFile(stat func(string) (os.FileInfo, error), path string) bool {
	info, err := stat(path)
	return err == nil && !info.IsDir()
}
