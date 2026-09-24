package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// UpdateUnit is the transient systemd unit the handoff runs under, so its
// output can be read back with `journalctl -u free-proxy-update`.
const UpdateUnit = "free-proxy-update"

// FinishUpdate hands the rest of an in-place upgrade to the binary that was
// just installed: it runs `free-proxy install` (dependencies, environment file,
// service unit, restart) exactly as the install script does over SSH.
//
// The command must outlive this process, because the last thing it does is stop
// it. Under systemd that means a transient unit — a plain child, even detached,
// still sits in this service's cgroup and would be killed along with it.
// Elsewhere a new session is the best available answer.
//
// The command sleeps first so the caller's HTTP response and job record are
// written before the service goes down, and falls back to a plain restart if
// the install itself fails, rather than leaving the service stopped.
func FinishUpdate(ctx context.Context, logPath string) error {
	if err := prepareUpdateLog(logPath); err != nil {
		return err
	}
	// --quiet suppresses the credential summary: this output goes to a file,
	// and the password does not need a second copy on disk.
	script := fmt.Sprintf("sleep 3; %s install --quiet >>%s 2>&1 || %s",
		shellQuote(BinPath), shellQuote(logPath), restartCommand())

	if hasCommand("systemd-run") {
		cmd := exec.CommandContext(ctx, "systemd-run", "--collect", "--unit", UpdateUnit,
			"--description", "Free Proxy in-place update", "/bin/sh", "-c", script)
		if out, err := cmd.CombinedOutput(); err != nil {
			// Fall through to the detached child rather than failing: a systemd
			// host that refuses the transient unit can still be updated, it
			// just loses the cgroup isolation.
			if err := startDetached(script); err != nil {
				return fmt.Errorf("systemd-run failed (%s) and detached start failed: %w",
					strings.TrimSpace(string(out)), err)
			}
		}
		return nil
	}
	return startDetached(script)
}

// startDetached runs the handoff in its own session so it survives the stop
// signal aimed at this process.
func startDetached(script string) error {
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	if err := cmd.Start(); err != nil {
		return err
	}
	// Nothing here waits for the child: the process that would reap it is the
	// one the child is about to stop, and init adopts what it leaves behind.
	return cmd.Process.Release()
}

func restartCommand() string {
	if hasCommand("systemctl") {
		return "systemctl restart free-proxy.service"
	}
	return "rc-service free-proxy restart"
}

// prepareUpdateLog creates the log the handoff appends to, owner-readable only:
// the shell would otherwise create it under the service's umask.
func prepareUpdateLog(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open update log: %w", err)
	}
	return f.Close()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
