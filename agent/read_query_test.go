package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"command-relay-mcp/internal/proto"
)

func TestBuildReadQueryCommand_UsesFixedReadOperations(t *testing.T) {
	repo := t.TempDir()
	cases := []struct {
		name string
		in   ReadQueryParams
		bin  string
		dir  string
		want []string
	}{
		{
			name: "git status",
			in:   ReadQueryParams{Operation: readQueryGitStatus, RepositoryPath: repo},
			bin:  "git",
			dir:  repo,
			want: []string{"--no-optional-locks", "-C", repo, "-c", "core.fsmonitor=false", "--no-pager", "status", "--short", "--branch", "--untracked-files=normal"},
		},
		{
			name: "git diff pathspec is one argument",
			in:   ReadQueryParams{Operation: readQueryGitDiff, RepositoryPath: repo, Pathspec: "--help;touch marker", Staged: true},
			bin:  "git",
			dir:  repo,
			want: []string{"--no-optional-locks", "-C", repo, "-c", "core.fsmonitor=false", "--no-pager", "diff", "--no-ext-diff", "--no-textconv", "--cached", "--", "--help;touch marker"},
		},
		{
			name: "git log has a bounded count",
			in:   ReadQueryParams{Operation: readQueryGitLog, RepositoryPath: repo, Pathspec: "cmd/agent.go", Limit: 35},
			bin:  "git",
			dir:  repo,
			want: []string{"--no-optional-locks", "-C", repo, "-c", "core.fsmonitor=false", "--no-pager", "log", "--no-color", "--no-decorate", "--no-show-signature", "--format=%h%x09%ad%x09%s", "--date=short", "-n", "35", "--", "cmd/agent.go"},
		},
		{
			name: "docker container list selects fixed fields",
			in:   ReadQueryParams{Operation: readQueryDockerPS, IncludeAll: true, Limit: 12},
			bin:  "docker",
			want: []string{"ps", "--last", "12", "--format", "{{.ID}}\\t{{.Names}}\\t{{.Image}}\\t{{.Status}}\\t{{.Ports}}", "--all"},
		},
		{
			name: "docker inspect selects fixed state fields",
			in:   ReadQueryParams{Operation: readQueryDockerInspect, Container: "api_1"},
			bin:  "docker",
			want: []string{"inspect", "--format", "{{.Id}}\\t{{.Name}}\\t{{.Config.Image}}\\t{{.State.Status}}\\t{{.State.Running}}\\t{{.State.StartedAt}}", "api_1"},
		},
		{
			name: "docker logs duration and line limit",
			in:   ReadQueryParams{Operation: readQueryDockerLogs, Container: "api_1", Tail: 24, Since: "90m"},
			bin:  "docker",
			want: []string{"logs", "--tail", "24", "--since", "1h30m0s", "--timestamps", "api_1"},
		},
		{
			name: "systemd status selects safe properties",
			in:   ReadQueryParams{Operation: readQuerySystemdStatus, Unit: "command-relay-agent.service"},
			bin:  "systemctl",
			want: []string{"--user", "show", "--no-pager", "--property=Id,Description,LoadState,ActiveState,SubState,MainPID,ExecMainStatus,ActiveEnterTimestamp", "command-relay-agent.service"},
		},
		{
			name: "systemd unit listing is user scoped",
			in:   ReadQueryParams{Operation: readQuerySystemdUnits},
			bin:  "systemctl",
			want: []string{"--user", "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager"},
		},
		{
			name: "journal unit and since",
			in:   ReadQueryParams{Operation: readQueryJournal, Unit: "command-relay-agent.service", Since: "2026-09-01T00:00:00Z", Lines: 40},
			bin:  "journalctl",
			want: []string{"--user", "--unit", "command-relay-agent.service", "--since", "2026-09-01T00:00:00Z", "--lines", "40", "--output=short-iso", "--no-pager", "--quiet"},
		},
		{
			name: "tmux does not start a server",
			in:   ReadQueryParams{Operation: readQueryTmuxSessions},
			bin:  "tmux",
			want: []string{"-N", "list-sessions", "-F", "#{session_id}\\t#{session_name}\\t#{session_windows}\\t#{session_attached}"},
		},
		{
			name: "tmux pane listing is scoped to a session",
			in:   ReadQueryParams{Operation: readQueryTmuxPanes, Session: "work"},
			bin:  "tmux",
			want: []string{"-N", "list-panes", "-s", "-t", "work", "-F", "#{pane_id}\\t#{window_name}\\t#{pane_current_command}\\t#{pane_current_path}"},
		},
		{
			name: "tmux capture uses a pane ID and bounded history",
			in:   ReadQueryParams{Operation: readQueryTmuxCapture, PaneID: "%42", Lines: 70},
			bin:  "tmux",
			want: []string{"-N", "capture-pane", "-p", "-J", "-t", "%42", "-S", "-70"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rpcErr := buildReadQueryCommand(tc.in)
			if rpcErr != nil {
				t.Fatalf("buildReadQueryCommand: %+v", rpcErr)
			}
			if got.name != tc.bin || got.dir != tc.dir || !reflect.DeepEqual(got.args, tc.want) {
				t.Fatalf("query = (%q, %q, %q), want (%q, %q, %q)", got.name, got.dir, got.args, tc.bin, tc.dir, tc.want)
			}
		})
	}
}

func TestBuildReadQueryCommand_RejectsUnboundedOrMutatingInputs(t *testing.T) {
	repo := t.TempDir()
	cases := []ReadQueryParams{
		{Operation: "shell", RepositoryPath: repo},
		{Operation: readQueryGitStatus, RepositoryPath: ""},
		{Operation: readQueryDockerInspect, Container: "x; rm -rf /"},
		{Operation: readQueryDockerLogs, Container: "api", Tail: 501},
		{Operation: readQueryDockerLogs, Container: "api", Since: "168h1m"},
		{Operation: readQuerySystemdStatus, Unit: "--help"},
		{Operation: readQueryJournal, Unit: "journal", Since: "-n all"},
		{Operation: readQueryJournal, Unit: "journal", Since: time.Now().Add(-31 * 24 * time.Hour).UTC().Format(time.RFC3339)},
		{Operation: readQueryTmuxPanes, Session: "session; new-session"},
		{Operation: readQueryTmuxCapture, PaneID: "session:0.0"},
	}
	for _, in := range cases {
		if _, rpcErr := buildReadQueryCommand(in); rpcErr == nil || rpcErr.Code != proto.ErrInvalidRequest {
			t.Errorf("buildReadQueryCommand(%+v) error = %v, want invalid_request", in, rpcErr)
		}
	}
}

func TestReadQuery_RejectsCallerSuppliedShellAndUnknownFields(t *testing.T) {
	called := false
	h := &ReadQueryHandlers{runCommand: func(context.Context, readQueryCommand) (ReadQueryResult, *proto.RPCError) {
		called = true
		return ReadQueryResult{}, nil
	}}
	for _, raw := range []string{
		`{"operation":"command_exec","command":"touch /tmp/should-not-exist"}`,
		`{"operation":"docker_ps","unexpected":"argument"}`,
	} {
		if _, rpcErr := h.Query(context.Background(), json.RawMessage(raw)); rpcErr == nil || rpcErr.Code != proto.ErrInvalidRequest {
			t.Errorf("Query(%s) error = %v, want invalid_request", raw, rpcErr)
		}
	}
	if called {
		t.Fatal("invalid input reached the command runner")
	}
}

func TestReadQueryCommand_ReturnsNonzeroExitAndCapsOutput(t *testing.T) {
	h := &ReadQueryHandlers{runCommand: func(_ context.Context, got readQueryCommand) (ReadQueryResult, *proto.RPCError) {
		if got.name != "docker" || !reflect.DeepEqual(got.args, []string{"ps", "--last", "100", "--format", "{{.ID}}\\t{{.Names}}\\t{{.Image}}\\t{{.Status}}\\t{{.Ports}}"}) {
			return ReadQueryResult{}, &proto.RPCError{Code: proto.ErrInternal, Message: "unexpected command"}
		}
		code := 7
		return ReadQueryResult{Stdout: "container", ExitCode: &code, StdoutTruncated: true}, nil
	}}
	params, _ := json.Marshal(ReadQueryParams{Operation: readQueryDockerPS})
	value, rpcErr := h.Query(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("Query: %+v", rpcErr)
	}
	got := value.(ReadQueryResult)
	if got.ExitCode == nil || *got.ExitCode != 7 || !got.StdoutTruncated {
		t.Fatalf("result = %+v, want nonzero exit and truncation", got)
	}
}

func TestReadQueryGitHub_UsesGETAndOnlyFixedAPIPaths(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/acme/project/issues/17" {
			t.Errorf("path = %s, want fixed issue path", r.URL.Path)
		}
		if r.Header.Get("X-GitHub-Api-Version") != githubAPIVersion {
			t.Errorf("API version = %q", r.Header.Get("X-GitHub-Api-Version"))
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization header was not set from the configured token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":17,"title":"Example","body":"text","state":"open","html_url":"https://github.com/acme/project/issues/17"}`))
	}))
	defer server.Close()
	t.Setenv("GH_TOKEN", "test-token")
	h := NewReadQueryHandlers()
	h.apiBase = server.URL
	h.httpClient = server.Client()
	params, _ := json.Marshal(ReadQueryParams{Operation: readQueryGitHubIssue, Owner: "acme", Repository: "project", Number: 17})
	value, rpcErr := h.Query(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("Query: %+v", rpcErr)
	}
	got := value.(ReadQueryResult)
	if !strings.Contains(got.Stdout, `"number":17`) || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("result = %+v", got)
	}
}

func TestReadQueryGitHub_RejectsPathInjection(t *testing.T) {
	h := NewReadQueryHandlers()
	params, _ := json.Marshal(ReadQueryParams{Operation: readQueryGitHubRepo, Owner: "acme/evil", Repository: "project"})
	if _, rpcErr := h.Query(context.Background(), params); rpcErr == nil || rpcErr.Code != proto.ErrInvalidRequest {
		t.Fatalf("Query error = %v, want invalid_request", rpcErr)
	}
}

func TestReadQueryGitStatusDoesNotWriteTheIndex(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "-c", "user.name=Read Query Test", "-c", "user.email=read-query@example.invalid", "commit", "--allow-empty", "-qm", "initial"},
	} {
		cmd := exec.Command(git, args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git setup failed: %v (%s)", err, output)
		}
	}
	index := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatalf("read index before: %v", err)
	}
	query, rpcErr := buildReadQueryCommand(ReadQueryParams{Operation: readQueryGitStatus, RepositoryPath: repo})
	if rpcErr != nil {
		t.Fatalf("build query: %+v", rpcErr)
	}
	result, rpcErr := runReadQueryCommand(context.Background(), query)
	if rpcErr != nil {
		t.Fatalf("run query: %+v", rpcErr)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("git status result = %+v", result)
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatalf("read index after: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("git_status changed the repository index")
	}
	if _, err := os.Stat(index + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git index lock exists after query: %v", err)
	}
}

func TestLimitedOutput_TracksTruncationWithoutGrowing(t *testing.T) {
	var out limitedOutput
	out.limit = 4
	if n, err := out.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("Write = (%d, %v)", n, err)
	}
	if out.buffer.String() != "abcd" || !out.truncated {
		t.Fatalf("limited output = %q truncated=%v", out.buffer.String(), out.truncated)
	}
}
