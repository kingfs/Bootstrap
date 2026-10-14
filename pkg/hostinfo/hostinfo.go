// Package hostinfo 探测当前运行环境：操作系统、架构、发行版、shell、包管理器等。
//
// 探测结果以值对象 Platform 返回，显式传给下游（不做全局单例），便于测试注入。
package hostinfo

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Platform 描述当前机器环境。
type Platform struct {
	OS          string   // linux | darwin | windows
	Arch        string   // amd64 | arm64 | 386 | ...
	Distro      string   // ubuntu / debian / fedora / arch / alpine ...（仅 linux）
	DistroVer   string   // 24.04 之类
	DistroLike  []string // ID_LIKE 列表
	Shell       string   // bash | zsh | fish | pwsh | cmd
	HomeDir     string
	IsWSL       bool
	IsContainer bool
	IsRoot      bool
	HasSudo     bool
	// PackageManagers 是探测到的系统包管理器可执行名（apt-get/dnf/pacman/brew/winget/scoop/choco...）。
	PackageManagers []string
}

// Env 是探测过程需要的全部外部输入，便于测试时替换。
type Env struct {
	GOOS     string
	GOARCH   string
	Getenv   func(string) string
	LookPath func(string) (string, error)
	ReadFile func(string) ([]byte, error)
	Stat     func(string) (os.FileInfo, error)
	HomeDir  string
	Euid     int
}

// OSEnv 返回基于真实系统的探测输入。
func OSEnv() Env {
	return Env{
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Getenv:   os.Getenv,
		LookPath: exec.LookPath,
		ReadFile: os.ReadFile,
		Stat:     os.Stat,
		HomeDir:  userHomeDir(),
		Euid:     os.Geteuid(),
	}
}

// Detect 使用真实环境探测。
func Detect() Platform { return DetectWith(OSEnv()) }

// DetectWith 使用给定输入探测，便于单元测试在不改机器的情况下验证各平台分支。
func DetectWith(env Env) Platform {
	if env.GOOS == "" {
		env.GOOS = runtime.GOOS
	}
	if env.GOARCH == "" {
		env.GOARCH = runtime.GOARCH
	}
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	if env.LookPath == nil {
		env.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	}
	if env.ReadFile == nil {
		env.ReadFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	}
	if env.Stat == nil {
		env.Stat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	}
	if env.HomeDir == "" {
		env.HomeDir = userHomeDir()
	}

	p := Platform{
		OS:      env.GOOS,
		Arch:    normalizeArch(env.GOARCH),
		HomeDir: env.HomeDir,
		IsRoot:  env.Euid == 0,
	}

	if p.OS == "linux" {
		applyOSRelease(&p, env)
		p.IsContainer = isContainer(env)
		p.IsWSL = env.Getenv("WSL_DISTRO_NAME") != "" || strings.Contains(strings.ToLower(readTrimmed(env, "/proc/version")), "microsoft")
	}

	p.Shell = detectShell(env)
	_, sudoErr := env.LookPath("sudo")
	p.HasSudo = sudoErr == nil
	p.PackageManagers = detectPackageManagers(env)
	return p
}

func applyOSRelease(p *Platform, env Env) {
	data, err := env.ReadFile("/etc/os-release")
	if err != nil {
		return
	}
	fields := parseOSRelease(string(data))
	p.Distro = fields["ID"]
	p.DistroVer = fields["VERSION_ID"]
	if like := fields["ID_LIKE"]; like != "" {
		p.DistroLike = strings.Fields(like)
	}
}

func parseOSRelease(content string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		out[strings.TrimSpace(key)] = val
	}
	return out
}

func readTrimmed(env Env, path string) string {
	data, err := env.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func isContainer(env Env) bool {
	if _, err := env.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := env.Stat("/run/.containerenv"); err == nil {
		return true
	}
	cgroup := readTrimmed(env, "/proc/1/cgroup")
	return strings.Contains(cgroup, "docker") || strings.Contains(cgroup, "containerd") || strings.Contains(cgroup, "kubepods")
}

func detectShell(env Env) string {
	if env.GOOS == "windows" {
		if env.Getenv("PSModulePath") != "" || env.Getenv("POWERSHELL_DISTRIBUTION_CHANNEL") != "" {
			return "pwsh"
		}
		return "cmd"
	}
	if s := strings.TrimSpace(env.Getenv("SHELL")); s != "" {
		return filepath.Base(s)
	}
	if env.Getenv("FISH_VERSION") != "" {
		return "fish"
	}
	if env.Getenv("ZSH_VERSION") != "" {
		return "zsh"
	}
	if env.Getenv("BASH_VERSION") != "" {
		return "bash"
	}
	// 以上都拿不到时（典型：`docker run -it ubuntu bash`，容器里没有 SHELL），
	// 退回到"这台机器上这个人登录用的 shell"。拿不到就老老实实返回空串，
	// 由调用方跳过 shell 集成——绝不猜一个可能错的，然后去写人家的 rc。
	if env.GOOS == "linux" {
		if s := loginShellFromPasswd(env); s != "" {
			return s
		}
		if s := parentProcessName(env); s != "" {
			return s
		}
	}
	return ""
}

// loginShellFromPasswd 从 /etc/passwd 里找当前用户的登录 shell。
//
// 只认"确实像 shell"的取值：nologin/false 这类占位符返回空，
// 否则我们会去给一个不允许登录的账号写 rc 文件。
func loginShellFromPasswd(env Env) string {
	data, err := env.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	user := strings.TrimSpace(env.Getenv("USER"))
	if user == "" {
		user = strings.TrimSpace(env.Getenv("LOGNAME"))
	}
	uid := strconv.Itoa(env.Euid)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			continue
		}
		// 用户名与 uid 任一匹配即可：容器里 USER 常常也没设置，此时只能靠 uid。
		if (user == "" || fields[0] != user) && fields[2] != uid {
			continue
		}
		if s := shellBase(fields[6]); s != "" {
			return s
		}
	}
	return ""
}

// parentProcessName 从 /proc 读出父进程名（bs 由哪个 shell 启动的）。
//
// /etc/passwd 里记的是"登录 shell"，而用户当前可能在另一个 shell 里敲命令
// （例如登录 shell 是 bash、实际在 zsh 里跑 bs），父进程名更贴近当下事实。
func parentProcessName(env Env) string {
	status, err := env.ReadFile("/proc/self/status")
	if err != nil {
		return ""
	}
	ppid := ""
	for _, line := range strings.Split(string(status), "\n") {
		if rest, ok := strings.CutPrefix(line, "PPid:"); ok {
			ppid = strings.TrimSpace(rest)
			break
		}
	}
	if ppid == "" || ppid == "0" {
		return ""
	}
	comm, err := env.ReadFile("/proc/" + ppid + "/comm")
	if err != nil {
		return ""
	}
	return shellBase(string(comm))
}

// shellBase 把 shell 路径/名字归一化；不是我们认识的 shell 就返回空串。
func shellBase(raw string) string {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(raw)))
	switch name {
	case "bash", "zsh", "fish", "sh", "dash", "ksh":
		return name
	default:
		return ""
	}
}

// candidateManagers 是各平台需要探测的包/版本管理器，顺序决定输出顺序。
var candidateManagers = []struct {
	bin  string
	goos []string
}{
	{"apt-get", []string{"linux"}},
	{"dnf", []string{"linux"}},
	{"yum", []string{"linux"}},
	{"pacman", []string{"linux"}},
	{"zypper", []string{"linux"}},
	{"apk", []string{"linux"}},
	{"brew", []string{"linux", "darwin"}},
	{"port", []string{"darwin"}},
	{"winget", []string{"windows"}},
	{"scoop", []string{"windows"}},
	{"choco", []string{"windows"}},
}

func detectPackageManagers(env Env) []string {
	var out []string
	for _, c := range candidateManagers {
		if !contains(c.goos, env.GOOS) {
			continue
		}
		if _, err := env.LookPath(c.bin); err == nil {
			out = append(out, c.bin)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// normalizeArch 把 Go 的架构名统一为 amd64/arm64/386/arm/riscv64。
func normalizeArch(a string) string {
	switch a {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	case "i386", "i686":
		return "386"
	default:
		return a
	}
}

func userHomeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE")
}

// ArchAliases 返回某个架构在各项目命名里的常见写法，供 release 资产模板匹配使用。
func ArchAliases(arch string) []string {
	switch arch {
	case "amd64":
		return []string{"amd64", "x86_64", "x64"}
	case "arm64":
		return []string{"arm64", "aarch64"}
	case "386":
		return []string{"386", "i386", "i686", "x86"}
	case "arm":
		return []string{"arm", "armv7", "armhf"}
	default:
		return []string{arch}
	}
}

// OSAliases 返回某个系统在各项目命名里的常见写法。
func OSAliases(goos string) []string {
	switch goos {
	case "darwin":
		return []string{"darwin", "macos", "osx", "mac"}
	case "windows":
		return []string{"windows", "win", "win32"}
	default:
		return []string{"linux"}
	}
}
