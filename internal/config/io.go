package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kingfs/Bootstrap/pkg/fsx"
)

const defaultHeader = `# Bootstrap (bs) 配置文件
#
# 优先级（后者覆盖前者）：
#   内置默认 < 系统级 < 用户级 < 项目级(.bootstrap.yaml) < 环境变量(BS_*) < 命令行 flag
#
# 字段参考：docs/configuration.md
# 查看生效值：bs config show
`

// Marshal 把配置序列化为 YAML（带说明头）。
func Marshal(c *Config) ([]byte, error) {
	body, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return append([]byte(defaultHeader), body...), nil
}

// WriteDefault 生成默认配置文件。
//
// force 为 false 时若文件已存在则不做任何修改（返回 created=false），保证幂等无副作用；
// force 为 true 时会先备份原文件。
func WriteDefault(paths Paths, force bool) (path string, created bool, err error) {
	path = paths.ConfigFile()
	if fsx.IsFile(path) {
		if !force {
			return path, false, nil
		}
		if _, err := fsx.BackupFile(path, timestamp()); err != nil {
			return path, false, err
		}
	}
	cfg := Defaults()
	applyDerived(&cfg, paths)
	data, err := Marshal(&cfg)
	if err != nil {
		return path, false, err
	}
	if err := fsx.AtomicWriteFile(path, data, 0o644); err != nil {
		return path, false, err
	}
	return path, true, nil
}

// timestamp 返回用于备份文件后缀的 UTC 时间戳。
func timestamp() string { return time.Now().UTC().Format("20060102T150405") }
