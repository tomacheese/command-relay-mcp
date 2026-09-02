// Command agent is the Agent entry point. It lives in its own
// subdirectory for the same reason as the Gateway's cmd package.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"command-relay-mcp/agent"
	"command-relay-mcp/internal/backend"
	"command-relay-mcp/internal/proto"
	"command-relay-mcp/internal/selfupdate"
	"command-relay-mcp/internal/version"
	"github.com/landlock-lsm/go-landlock/landlock"
)

// versionRequested reports whether args (os.Args) asked for --version.
// Checked before the --landlock-exec branch so the two never collide.
func versionRequested(args []string) bool {
	return len(args) >= 2 && args[1] == "--version"
}

func main() {
	if versionRequested(os.Args) {
		fmt.Println(version.Version)
		return
	}

	// Hidden self-re-exec target for command.read's sandbox. This must
	// be the ONLY code path in this binary
	// that ever calls the Landlock API: Landlock restrictions apply to
	// the calling process for the rest of its life and cannot be
	// lifted, so applying them here — in a short-lived child that
	// immediately execs the real command — keeps the long-running Agent
	// itself unsandboxed for every other command.exec call. The child's
	// user+network namespaces already exist by the time this runs
	// (created by SandboxedBackend's SysProcAttr.Cloneflags at process
	// creation), so no unshare() call is needed here for those.
	if len(os.Args) >= 3 && os.Args[1] == "--landlock-exec" && os.Args[2] == "--" {
		landlockExecMain(os.Args[3:])
		return // unreachable on success: syscall.Exec replaces this process
	}

	cfg := agent.LoadConfig()
	mustEnv("DEVICE_ID")
	mustEnv("DEVICE_SECRET")
	mustEnv("GATEWAY_URL")

	hist, err := agent.OpenHistoryStore(envOr("HISTORY_DB_PATH", "agent-history.db"))
	if err != nil {
		log.Fatalf("agent: open history store: %v", err)
	}
	defer hist.Close()

	mgr := agent.NewManager(backend.NewLinuxBackend(cfg.DefaultShell), cfg.StdoutBufferBytes, cfg.StderrBufferBytes, cfg.FinishedProcessTTL)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	mgr.StartGC(ctx, time.Minute)
	hist.StartGC(ctx, cfg.HistoryRetention, time.Hour)
	selfupdate.Start(ctx, selfupdate.Options{
		Enabled:        cfg.AutoUpdateEnabled,
		Interval:       cfg.AutoUpdateInterval,
		CurrentVersion: cfg.AgentVersion,
	})

	selfPath, err := os.Executable()
	if err != nil {
		log.Fatalf("agent: os.Executable: %v", err)
	}
	sandboxBackend := backend.NewSandboxedBackend(selfPath, cfg.DefaultShell)
	sandboxSupported := agent.ProbeSandbox(sandboxBackend)
	var sandboxMgr *agent.Manager
	if sandboxSupported {
		sandboxMgr = agent.NewManager(sandboxBackend, cfg.StdoutBufferBytes, cfg.StderrBufferBytes, cfg.FinishedProcessTTL)
		sandboxMgr.StartGC(ctx, time.Minute)
	} else {
		log.Print("agent: sandbox (Landlock) not available on this kernel; command_read will return unsupported")
	}

	cmdHandlers := agent.NewCommandHandlers(mgr, sandboxMgr, hist, cfg.DeviceID, 30_000)
	procHandlers := agent.NewProcessHandlers(mgr, sandboxMgr, hist, cfg.DeviceID)
	execHandlers := agent.NewExecutionHandlers(hist)
	fileHandlers := agent.NewFileHandlers()

	d := agent.NewDispatcher()
	d.Handle(proto.MethodExecutionList, execHandlers.List)
	d.Handle(proto.MethodExecutionGet, execHandlers.Get)
	d.Handle(proto.MethodCommandExec, cmdHandlers.Exec)
	d.Handle(proto.MethodCommandRead, cmdHandlers.Read)
	d.Handle(proto.MethodProcessStart, procHandlers.Start)
	d.Handle(proto.MethodProcessRead, procHandlers.Read)
	d.Handle(proto.MethodProcessWrite, procHandlers.Write)
	d.Handle(proto.MethodProcessWait, procHandlers.Wait)
	d.Handle(proto.MethodProcessTerminate, procHandlers.Terminate)
	d.Handle(proto.MethodProcessList, procHandlers.List)
	d.Handle(proto.MethodDevicePing, agent.Ping)
	d.Handle(proto.MethodFileRead, fileHandlers.Read)
	d.Handle(proto.MethodFileStat, fileHandlers.Stat)
	d.Handle(proto.MethodFileList, fileHandlers.List)
	d.Handle(proto.MethodFileWrite, fileHandlers.Write)
	d.Handle(proto.MethodFileMove, fileHandlers.Move)
	d.Handle(proto.MethodFileDelete, fileHandlers.Delete)
	d.Handle(proto.MethodDirectoryCreate, fileHandlers.CreateDirectory)

	caps := proto.Capabilities{CommandExec: true, CommandRead: sandboxSupported, Process: true, Filesystem: true}
	conn := agent.NewConnection(cfg, d, caps)

	log.Printf("agent: connecting to %s as device %q", cfg.GatewayURL, cfg.DeviceID)
	runErr := conn.Run(ctx)

	// A graceful stop (SIGTERM/SIGINT) terminates every process tree
	// this Agent started, not just the Agent itself. This covers
	// graceful stops; systemd's default
	// KillMode=control-group (see deploy/systemd/command-relay-agent.service)
	// covers the SIGKILL/crash case this code can't catch.
	log.Print("agent: shutting down, terminating managed process trees")
	mgr.TerminateAll(5000)
	if sandboxMgr != nil {
		sandboxMgr.TerminateAll(5000)
	}

	if runErr != nil && ctx.Err() == nil {
		log.Fatalf("agent: %v", runErr)
	}
}

// landlockExecMain applies a strict, non-BestEffort Landlock ruleset —
// read-only over the whole filesystem except a private per-invocation
// scratch directory — then execs the real command, replacing this
// process. It never returns on success; on any setup failure it exits
// with backend.SandboxSetupFailedExitCode instead of falling through to
// running the command unsandboxed.
//
// The exit code alone cannot signal "setup failed" to the parent. A
// successful syscall.Exec fully replaces this process image with the
// sandboxed command's, so the parent would otherwise be unable to tell
// that command legitimately exiting 111 apart from this wrapper failing
// setup — a command's own non-zero exit is never a protocol error. A
// status pipe, inherited on the reserved backend.statusPipeFD, carries
// that signal out-of-band instead: marked close-on-exec here, so a
// successful exec closes it automatically (silence = success), while
// every failure path below writes to it before exiting.
func landlockExecMain(execArgv []string) {
	statusFD := os.NewFile(3, "sandbox-status")
	syscall.CloseOnExec(3)
	fail := func() {
		statusFD.Write([]byte{1})
		os.Exit(backend.SandboxSetupFailedExitCode)
	}

	if len(execArgv) == 0 {
		fail()
	}
	// The parent (backend.SandboxedBackend.Start) creates and later removes
	// the scratch directory — this process's own image is gone by the time
	// the command exits (syscall.Exec replaces it), so only the parent can
	// clean it up (the scratch area is "writable, ephemeral").
	scratchDir := os.Getenv("RC_SANDBOX_SCRATCH_DIR")
	if scratchDir == "" {
		fail()
	}
	linkHostHomeEntries(os.Getenv("HOME"), scratchDir)
	os.Setenv("TMPDIR", scratchDir)
	os.Setenv("HOME", scratchDir)

	// Strict mode only: BestEffort() would silently downgrade protection
	// on partial kernel support, which is forbidden for the same reason
	// falling back to command.exec is forbidden. Network denial is
	// already in place by this point —
	// SandboxedBackend joined a fresh network namespace at process
	// creation — so RestrictPaths only needs to cover the filesystem.
	// /dev/null is explicitly writable: it's a no-op sink that changes no
	// persistent state, so denying writes to it (the RODirs("/") default)
	// breaks the common `>/dev/null` redirect for no security benefit.
	if err := landlock.V4.RestrictPaths(landlock.RODirs("/"), landlock.RWDirs(scratchDir), landlock.RWFiles("/dev/null")); err != nil {
		fail()
	}

	path, lookErr := exec.LookPath(execArgv[0])
	if lookErr != nil {
		fail()
	}
	if err := syscall.Exec(path, execArgv, os.Environ()); err != nil {
		fail()
	}
}

// hostHomeEntriesToLink are the login-shell profile scripts and XDG base
// directories symlinked from the real host HOME into the sandbox's
// scratch HOME (see linkHostHomeEntries). This list is deliberately
// generic — shell profile filenames plus the XDG Base Directory
// names — rather than any specific tool manager's own directory, so it
// keeps working for whichever user-level tool manager (mise, asdf, ...)
// the host happens to use.
var hostHomeEntriesToLink = []string{
	".profile", ".bash_profile", ".bash_login", ".bashrc",
	".zshenv", ".zprofile", ".zshrc",
	".config", ".local", ".cache",
}

// linkHostHomeEntries symlinks hostHomeEntriesToLink's existing entries
// from realHome into scratchDir, so a login shell run with HOME=scratchDir
// still sources the host's ~/.profile and resolves the same PATH/XDG
// config a normal command_exec session would — without making scratchDir
// itself an alias for the whole real HOME (issue #31). Landlock is applied
// after this and after HOME is repointed to scratchDir, so it resolves
// these through the symlink to the real, RODirs("/")-covered path: reads
// succeed, writes still don't, and any new top-level entry a command
// creates under $HOME (not one of these names) lands in scratchDir itself,
// which stays writable.
//
// Best-effort: a missing realHome, or an entry that doesn't exist or
// can't be symlinked, is skipped rather than failing sandbox setup — a
// host without a user-level tool manager (or without HOME set at all)
// must see no behavior change.
func linkHostHomeEntries(realHome, scratchDir string) {
	if realHome == "" {
		return
	}
	for _, name := range hostHomeEntriesToLink {
		target := filepath.Join(realHome, name)
		if _, err := os.Lstat(target); err != nil {
			continue
		}
		os.Symlink(target, filepath.Join(scratchDir, name))
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("agent: required env var %s not set", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
