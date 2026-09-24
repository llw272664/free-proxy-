package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/masteralanlab/free-proxy/internal/platform"
)

// SelfUpdateSupport reports why this process cannot replace its own binary, or
// nil when it can. The console asks before showing an update button, so the
// reasons are written for the operator rather than for a log: each one names
// what is wrong with this particular install.
func SelfUpdateSupport() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("自助更新仅支持 Linux 安装（当前系统 %s）", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("自助更新需要 root 权限，当前进程以 uid %d 运行", os.Geteuid())
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法确定当前程序路径：%w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if exe != platform.BinPath {
		return fmt.Errorf("当前进程运行自 %s，不是 %s 安装的服务，请用安装脚本更新", exe, platform.BinPath)
	}
	// The replacement is a rename inside this directory, so the directory —
	// not the file — is what has to be writable.
	probe, err := os.CreateTemp(filepath.Dir(platform.BinPath), ".free-proxy-probe-*")
	if err != nil {
		return fmt.Errorf("%s 不可写：%w", filepath.Dir(platform.BinPath), err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return nil
}
