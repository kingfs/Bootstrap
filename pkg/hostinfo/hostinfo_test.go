package hostinfo

import (
	"os"
	"os/exec"
	"testing"
)

const ubuntuOSRelease = `PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
ID=ubuntu
ID_LIKE=debian
`

func fakeEnv(goos string, files map[string]string, lookPath ...string) Env {
	available := map[string]bool{}
	for _, p := range lookPath {
		available[p] = true
	}
	return Env{
		GOOS:   goos,
		GOARCH: "arm64",
		Getenv: func(k string) string {
			if k == "SHELL" {
				return "/bin/zsh"
			}
			return ""
		},
		LookPath: func(name string) (string, error) {
			if available[name] {
				return "/usr/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		},
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		},
		Stat: func(p string) (os.FileInfo, error) {
			if _, ok := files[p]; ok {
				return nil, nil
			}
			return nil, os.ErrNotExist
		},
		HomeDir: "/home/kingfs",
		Euid:    1000,
	}
}

func TestDetectLinuxUbuntu(t *testing.T) {
	p := DetectWith(fakeEnv("linux", map[string]string{
		"/etc/os-release": ubuntuOSRelease,
	}, "apt-get", "brew", "sudo"))

	if p.OS != "linux" || p.Arch != "arm64" {
		t.Fatalf("OS/Arch = %s/%s, want linux/arm64", p.OS, p.Arch)
	}
	if p.Distro != "ubuntu" || p.DistroVer != "24.04" {
		t.Errorf("Distro = %q %q, want ubuntu 24.04", p.Distro, p.DistroVer)
	}
	if len(p.DistroLike) != 1 || p.DistroLike[0] != "debian" {
		t.Errorf("DistroLike = %v, want [debian]", p.DistroLike)
	}
	if p.Shell != "zsh" {
		t.Errorf("Shell = %q, want zsh", p.Shell)
	}
	if p.IsRoot {
		t.Error("IsRoot = true, want false (euid 1000)")
	}
	if !p.HasSudo {
		t.Error("HasSudo = false, want true")
	}
	want := []string{"apt-get", "brew"}
	if len(p.PackageManagers) != len(want) {
		t.Fatalf("PackageManagers = %v, want %v", p.PackageManagers, want)
	}
	for i := range want {
		if p.PackageManagers[i] != want[i] {
			t.Errorf("PackageManagers = %v, want %v", p.PackageManagers, want)
		}
	}
	if p.HomeDir != "/home/kingfs" {
		t.Errorf("HomeDir = %q", p.HomeDir)
	}
}

func TestDetectLinuxContainerAndWSL(t *testing.T) {
	p := DetectWith(fakeEnv("linux", map[string]string{
		"/etc/os-release": ubuntuOSRelease,
		"/.dockerenv":     "",
	}))
	if !p.IsContainer {
		t.Error("IsContainer = false, want true with /.dockerenv")
	}
	if p.IsWSL {
		t.Error("IsWSL = true, want false")
	}
}

func TestDetectDarwin(t *testing.T) {
	env := fakeEnv("darwin", map[string]string{}, "brew")
	env.Getenv = func(k string) string {
		if k == "SHELL" {
			return "/bin/bash"
		}
		return ""
	}
	p := DetectWith(env)
	if p.OS != "darwin" {
		t.Fatalf("OS = %q, want darwin", p.OS)
	}
	if p.Distro != "" {
		t.Errorf("Distro = %q, want empty on darwin", p.Distro)
	}
	if len(p.PackageManagers) != 1 || p.PackageManagers[0] != "brew" {
		t.Errorf("PackageManagers = %v, want [brew]", p.PackageManagers)
	}
}

func TestDetectWindowsShell(t *testing.T) {
	env := fakeEnv("windows", map[string]string{}, "winget", "scoop")
	env.Getenv = func(k string) string {
		if k == "PSModulePath" {
			return `C:\Program Files\PowerShell\Modules`
		}
		return ""
	}
	p := DetectWith(env)
	if p.Shell != "pwsh" {
		t.Errorf("Shell = %q, want pwsh", p.Shell)
	}
	if len(p.PackageManagers) != 2 {
		t.Errorf("PackageManagers = %v, want 2 entries", p.PackageManagers)
	}

	// 没有 PowerShell 痕迹时判定为 cmd。
	env.Getenv = func(string) string { return "" }
	if got := DetectWith(env).Shell; got != "cmd" {
		t.Errorf("Shell = %q, want cmd", got)
	}
}

// TestDetectShellFallsBackToPasswd：容器里常常没有 $SHELL（`docker run -it ubuntu bash`
// 就是如此）。这时改看 /etc/passwd 里当前用户的登录 shell，别把 shell 探测成空串——
// 空串会让 `bs init` 的 shell 集成整段失败。
func TestDetectShellFallsBackToPasswd(t *testing.T) {
	env := fakeEnv("linux", map[string]string{
		"/etc/os-release": ubuntuOSRelease,
		"/etc/passwd":     "root:x:0:0:root:/root:/bin/bash\ndaemon:x:1:1::/:/usr/sbin/nologin\n",
	})
	env.Getenv = func(string) string { return "" }
	env.Euid = 0
	if p := DetectWith(env); p.Shell != "bash" {
		t.Errorf("Shell = %q, want bash（$SHELL 缺失时应看 /etc/passwd）", p.Shell)
	}

	// 用户名优先于 uid：同一个 uid 匹配不上时按 $USER 找。
	env.Euid = 1000
	env.Getenv = func(k string) string {
		if k == "USER" {
			return "daemon"
		}
		return ""
	}
	// daemon 的登录 shell 是 nologin（不是 shell）→ 不能被当成 shell 返回。
	if p := DetectWith(env); p.Shell != "" {
		t.Errorf("Shell = %q, want empty（nologin 不是 shell）", p.Shell)
	}
}

// TestDetectShellFallsBackToParentProcess：/etc/passwd 也给不出答案时，
// 看父进程是谁（bs 是被哪个 shell 拉起来的）。
func TestDetectShellFallsBackToParentProcess(t *testing.T) {
	env := fakeEnv("linux", map[string]string{
		"/etc/os-release":   ubuntuOSRelease,
		"/proc/self/status": "Name:\tbs\nPPid:\t4242\n",
		"/proc/4242/comm":   "zsh\n",
	})
	env.Getenv = func(string) string { return "" }
	if p := DetectWith(env); p.Shell != "zsh" {
		t.Errorf("Shell = %q, want zsh（父进程是 zsh）", p.Shell)
	}

	// 什么都拿不到时必须是空串（调用方据此跳过 shell 集成），不能瞎猜 bash。
	bare := fakeEnv("linux", map[string]string{})
	bare.Getenv = func(string) string { return "" }
	if p := DetectWith(bare); p.Shell != "" {
		t.Errorf("Shell = %q, want empty（探测不到就如实报告）", p.Shell)
	}
}

func TestNormalizeArch(t *testing.T) {
	tests := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "i686": "386", "arm64": "arm64", "riscv64": "riscv64"}
	for in, want := range tests {
		if got := normalizeArch(in); got != want {
			t.Errorf("normalizeArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseOSReleaseTolerant(t *testing.T) {
	got := parseOSRelease("# comment\n\nID=fedora\nVERSION_ID=\"39\"\nID_LIKE=\"rhel centos\"\n")
	if got["ID"] != "fedora" || got["VERSION_ID"] != "39" {
		t.Errorf("parseOSRelease = %v", got)
	}
	if got["ID_LIKE"] != "rhel centos" {
		t.Errorf("ID_LIKE = %q", got["ID_LIKE"])
	}
}

func TestArchAndOSAliases(t *testing.T) {
	if aliases := ArchAliases("amd64"); len(aliases) < 2 || aliases[0] != "amd64" {
		t.Errorf("ArchAliases(amd64) = %v", aliases)
	}
	if aliases := OSAliases("darwin"); len(aliases) < 2 || aliases[0] != "darwin" {
		t.Errorf("OSAliases(darwin) = %v", aliases)
	}
	if aliases := OSAliases("windows"); len(aliases) < 2 {
		t.Errorf("OSAliases(windows) = %v", aliases)
	}
}
