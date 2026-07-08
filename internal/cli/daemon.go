package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// defaultPidPath returns ~/.config/omni-agent-hub/omni-agent-hub.pid.
func defaultPidPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/oah.pid"
	}
	return filepath.Join(home, ".config", "omni-agent-hub", "omni-agent-hub.pid")
}

// readPid reads the PID from the pid file and returns it, or 0 if missing/invalid.
func readPid() (int, error) {
	data, err := os.ReadFile(defaultPidPath())
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid pid file: %w", err)
	}
	return pid, nil
}

// writePid writes a PID to the pid file.
func writePid(pid int) error {
	pidPath := defaultPidPath()
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// removePid removes the pid file.
func removePid() {
	os.Remove(defaultPidPath())
}

// isRunning checks whether the given PID is a live omni-agent-hub process.
func isRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 tests existence without actually sending a signal.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// selfBinary returns the absolute path to the currently running binary,
// falling back to searching PATH for "oah" then "omni-agent-hub".
func selfBinary() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		return exe, nil
	}
	if p, e := exec.LookPath("oah"); e == nil {
		return p, nil
	}
	return exec.LookPath("omni-agent-hub")
}

func newStartCmd(opts *Opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the server as a background daemon",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Check if already running.
			if pid, err := readPid(); err == nil && isRunning(pid) {
				fmt.Printf("%s oah is already running %s\n", yellow("•"), dim(fmt.Sprintf("(pid %d)", pid)))
				return nil
			}

			bin, err := selfBinary()
			if err != nil {
				return fmt.Errorf("cannot find oah binary: %w", err)
			}

			// Build the child argument list: omni-agent-hub serve [flags]
			args := []string{"serve"}
			if opts.ConfigPath != "" {
				args = append(args, "--config", opts.ConfigPath)
			}
			if opts.LogFile != "" {
				args = append(args, "--log-file", opts.LogFile)
			}

			child := exec.Command(bin, args...)
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			// Detach stdio so the daemon doesn't hold the terminal.
			child.Stdin = nil
			child.Stdout = nil
			child.Stderr = nil

			if err := child.Start(); err != nil {
				return fmt.Errorf("failed to start daemon: %w", err)
			}

			pid := child.Process.Pid
			if err := writePid(pid); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write pid file: %v\n", err)
			}

			// Release the child so it doesn't become a zombie.
			child.Process.Release()

			fmt.Printf("%s oah started %s\n", okGlyph(), dim(fmt.Sprintf("(pid %d)", pid)))
			fmt.Printf("  %s %s\n", dim("pid file"), defaultPidPath())
			fmt.Printf("  %s %s\n", dim("logs    "), "oah logs -f")
			return nil
		},
	}
	return cmd
}

func newStopCmd(_ *Opts) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			pid, err := readPid()
			if err != nil {
				fmt.Printf("%s oah is not running %s\n", dim("•"), dim("(no pid file)"))
				return nil
			}
			if !isRunning(pid) {
				fmt.Printf("%s oah is not running %s\n", dim("•"), dim(fmt.Sprintf("(stale pid %d)", pid)))
				removePid()
				return nil
			}

			proc, err := os.FindProcess(pid)
			if err != nil {
				return fmt.Errorf("finding process %d: %w", pid, err)
			}

			sig := syscall.SIGTERM
			if force {
				sig = syscall.SIGKILL
			}

			if err := proc.Signal(sig); err != nil {
				return fmt.Errorf("sending signal to pid %d: %w", pid, err)
			}

			// Wait up to 5 seconds for the process to exit.
			for i := 0; i < 50; i++ {
				if !isRunning(pid) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}

			if isRunning(pid) {
				fmt.Printf("%s oah (pid %d) did not exit in time; use --force to kill\n", yellow("•"), pid)
				return nil
			}

			removePid()
			fmt.Printf("%s oah stopped %s\n", okGlyph(), dim(fmt.Sprintf("(pid %d)", pid)))
			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "send SIGKILL instead of SIGTERM")
	return cmd
}

func newRestartCmd(opts *Opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon (stop + start)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Stop.
			stopCmd := newStopCmd(opts)
			if err := stopCmd.RunE(stopCmd, nil); err != nil {
				return err
			}

			// Brief pause to let the port release.
			time.Sleep(500 * time.Millisecond)

			// Start.
			startCmd := newStartCmd(opts)
			return startCmd.RunE(startCmd, nil)
		},
	}
	return cmd
}

func newStatusCmd(_ *Opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the daemon is running",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			pid, err := readPid()
			if err != nil {
				fmt.Printf("%s oah is not running %s\n", dim("•"), dim("(no pid file)"))
				return nil
			}
			if !isRunning(pid) {
				fmt.Printf("%s oah is not running %s\n", dim("•"), dim(fmt.Sprintf("(stale pid %d)", pid)))
				removePid()
				return nil
			}
			fmt.Printf("%s oah is running %s\n", green("•"), dim(fmt.Sprintf("(pid %d)", pid)))
			fmt.Printf("  %s %s\n", dim("pid file"), defaultPidPath())
			return nil
		},
	}
	return cmd
}
