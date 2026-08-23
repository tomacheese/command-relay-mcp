// Command agent is the Agent entry point. It lives in its own
// subdirectory for the same reason as the Gateway's cmd package.
package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"command-relay-mcp/agent"
	"command-relay-mcp/internal/backend"
	"command-relay-mcp/internal/proto"
	"github.com/landlock-lsm/go-landlock/landlock"
)

func main() {
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

// landlockExecMain isolates mount propagation and remounts /proc for
// this process's own PID namespace, then applies a strict,
// non-BestEffort Landlock ruleset — read-only over the whole filesystem
// except a private per-invocation scratch directory — then execs the
// real command, replacing this process. It never returns on success; on
// any setup failure it exits with backend.SandboxSetupFailedExitCode
// instead of falling through to running the command unsandboxed.
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
	os.Setenv("TMPDIR", scratchDir)
	os.Setenv("HOME", scratchDir)

	// SandboxedBackend's CLONE_NEWNS gave this process its own mount
	// namespace, but the mount table it started with is still a copy of
	// the host's — including a shared-propagation flag that would
	// otherwise leak the following mount/remount back to the host. Make
	// it private first, then remount /proc so it reflects this
	// process's own PID namespace: left as the inherited host /proc,
	// procps-family tools (ps, etc.) fail to resolve their own PID for
	// any command that isn't PID 1 of the namespace (e.g. anything past
	// the first stage of a pipeline).
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fail()
	}
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		fail()
	}

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
