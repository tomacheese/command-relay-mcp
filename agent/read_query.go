package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"command-relay-mcp/internal/proto"
)

const (
	readQueryTimeout       = 20 * time.Second
	readQueryDefaultLines  = 100
	readQueryMaxLines      = 500
	readQueryMaxPathLength = 4096
	githubAPIBase          = "https://api.github.com"
	githubAPIVersion       = "2026-03-10"
)

const (
	readQueryGitStatus     = "git_status"
	readQueryGitDiff       = "git_diff"
	readQueryGitLog        = "git_log"
	readQueryDockerPS      = "docker_ps"
	readQueryDockerInspect = "docker_inspect"
	readQueryDockerLogs    = "docker_logs"
	readQuerySystemdStatus = "systemd_status"
	readQuerySystemdUnits  = "systemd_units"
	readQueryJournal       = "journal_query"
	readQueryTmuxSessions  = "tmux_sessions"
	readQueryTmuxPanes     = "tmux_panes"
	readQueryTmuxCapture   = "tmux_capture"
	readQueryGitHubRepo    = "github_repo"
	readQueryGitHubIssue   = "github_issue"
	readQueryGitHubPull    = "github_pull_request"
)

var (
	readQueryUnitPattern      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:@-]{0,254}$`)
	readQueryContainerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	readQuerySessionPattern   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	readQueryPanePattern      = regexp.MustCompile(`^%[0-9]{1,20}$`)
	readQueryOwnerPattern     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	readQueryRepoPattern      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

// ReadQueryParams selects a fixed inspection operation. It never carries a
// shell command or an executable name.
type ReadQueryParams struct {
	Operation      string `json:"operation"`
	RepositoryPath string `json:"repository_path,omitempty"`
	Pathspec       string `json:"pathspec,omitempty"`
	Staged         bool   `json:"staged,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	IncludeAll     bool   `json:"include_all,omitempty"`
	Container      string `json:"container,omitempty"`
	Tail           int    `json:"tail,omitempty"`
	Since          string `json:"since,omitempty"`
	Unit           string `json:"unit,omitempty"`
	Session        string `json:"session,omitempty"`
	PaneID         string `json:"pane_id,omitempty"`
	Lines          int    `json:"lines,omitempty"`
	Owner          string `json:"owner,omitempty"`
	Repository     string `json:"repository,omitempty"`
	Number         int    `json:"number,omitempty"`
}

// ReadQueryResult is the bounded output of one fixed read operation.
type ReadQueryResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

type readQueryCommand struct {
	name string
	args []string
	dir  string
}

type readQueryCommandRunner func(context.Context, readQueryCommand) (ReadQueryResult, *proto.RPCError)

// ReadQueryHandlers exposes bounded host and GitHub inspection operations.
type ReadQueryHandlers struct {
	runCommand readQueryCommandRunner
	apiBase    string
	httpClient *http.Client
}

func NewReadQueryHandlers() *ReadQueryHandlers {
	return &ReadQueryHandlers{
		runCommand: runReadQueryCommand,
		apiBase:    githubAPIBase,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL.Host != "api.github.com" {
					return http.ErrUseLastResponse
				}
				if len(via) >= 3 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
	}
}

// Query implements read.query. The operation determines the complete command
// and arguments; caller input is limited to validated paths and selectors.
func (h *ReadQueryHandlers) Query(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p ReadQueryParams
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: "invalid read query parameters"}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: "invalid read query parameters"}
	}

	if isGitHubReadQuery(p.Operation) {
		return h.queryGitHub(ctx, p)
	}
	cmd, rpcErr := buildReadQueryCommand(p)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return h.runCommand(ctx, cmd)
}

func isGitHubReadQuery(operation string) bool {
	return operation == readQueryGitHubRepo || operation == readQueryGitHubIssue || operation == readQueryGitHubPull
}

func invalidReadQuery(message string) *proto.RPCError {
	return &proto.RPCError{Code: proto.ErrInvalidRequest, Message: message}
}

func buildReadQueryCommand(p ReadQueryParams) (readQueryCommand, *proto.RPCError) {
	cmd := readQueryCommand{}
	switch p.Operation {
	case readQueryGitStatus, readQueryGitDiff, readQueryGitLog:
		repo, err := validateReadQueryDirectory(p.RepositoryPath)
		if err != nil {
			return cmd, invalidReadQuery("repository_path must be an existing directory")
		}
		cmd = readQueryCommand{name: "git", dir: repo}
		prefix := []string{"--no-optional-locks", "-C", repo, "-c", "core.fsmonitor=false", "--no-pager"}
		switch p.Operation {
		case readQueryGitStatus:
			if p.Pathspec != "" || p.Staged {
				return readQueryCommand{}, invalidReadQuery("git_status does not accept pathspec or staged")
			}
			cmd.args = append(prefix, "status", "--short", "--branch", "--untracked-files=normal")
		case readQueryGitDiff:
			if err := validatePathspec(p.Pathspec); err != nil {
				return readQueryCommand{}, invalidReadQuery("pathspec is invalid")
			}
			cmd.args = append(prefix, "diff", "--no-ext-diff", "--no-textconv")
			if p.Staged {
				cmd.args = append(cmd.args, "--cached")
			}
			cmd.args = append(cmd.args, "--")
			if p.Pathspec != "" {
				cmd.args = append(cmd.args, p.Pathspec)
			}
		case readQueryGitLog:
			limit, err := boundedInteger(p.Limit, 20, 200)
			if err != nil {
				return readQueryCommand{}, invalidReadQuery("limit must be between 1 and 200")
			}
			if err := validatePathspec(p.Pathspec); err != nil {
				return readQueryCommand{}, invalidReadQuery("pathspec is invalid")
			}
			cmd.args = append(prefix, "log", "--no-color", "--no-decorate", "--no-show-signature", "--format=%h%x09%ad%x09%s", "--date=short", "-n", strconv.Itoa(limit), "--")
			if p.Pathspec != "" {
				cmd.args = append(cmd.args, p.Pathspec)
			}
		}
	case readQueryDockerPS:
		limit, err := boundedInteger(p.Limit, readQueryDefaultLines, readQueryMaxLines)
		if err != nil {
			return cmd, invalidReadQuery("limit must be between 1 and 500")
		}
		cmd.name = "docker"
		cmd.args = []string{"ps", "--last", strconv.Itoa(limit), "--format", "{{.ID}}\\t{{.Names}}\\t{{.Image}}\\t{{.Status}}\\t{{.Ports}}"}
		if p.IncludeAll {
			cmd.args = append(cmd.args, "--all")
		}
	case readQueryDockerInspect:
		if !readQueryContainerPattern.MatchString(p.Container) {
			return cmd, invalidReadQuery("container must be a container name or ID")
		}
		cmd.name = "docker"
		cmd.args = []string{"inspect", "--format", "{{.Id}}\\t{{.Name}}\\t{{.Config.Image}}\\t{{.State.Status}}\\t{{.State.Running}}\\t{{.State.StartedAt}}", p.Container}
	case readQueryDockerLogs:
		if !readQueryContainerPattern.MatchString(p.Container) {
			return cmd, invalidReadQuery("container must be a container name or ID")
		}
		tail, err := boundedInteger(p.Tail, readQueryDefaultLines, readQueryMaxLines)
		if err != nil {
			return cmd, invalidReadQuery("tail must be between 1 and 500")
		}
		since, err := boundedDuration(p.Since, time.Hour, 7*24*time.Hour)
		if err != nil {
			return cmd, invalidReadQuery("since must be a duration between 1ns and 168h")
		}
		cmd.name = "docker"
		cmd.args = []string{"logs", "--tail", strconv.Itoa(tail), "--since", since, "--timestamps", p.Container}
	case readQuerySystemdStatus:
		if !readQueryUnitPattern.MatchString(p.Unit) {
			return cmd, invalidReadQuery("unit is invalid")
		}
		cmd.name = "systemctl"
		cmd.args = []string{"--user", "show", "--no-pager", "--property=Id,Description,LoadState,ActiveState,SubState,MainPID,ExecMainStatus,ActiveEnterTimestamp", p.Unit}
	case readQuerySystemdUnits:
		cmd.name = "systemctl"
		cmd.args = []string{"--user", "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager"}
	case readQueryJournal:
		if !readQueryUnitPattern.MatchString(p.Unit) {
			return cmd, invalidReadQuery("unit is invalid")
		}
		lines, err := boundedInteger(p.Lines, readQueryDefaultLines, readQueryMaxLines)
		if err != nil {
			return cmd, invalidReadQuery("lines must be between 1 and 500")
		}
		since, err := boundedJournalSince(p.Since)
		if err != nil {
			return cmd, invalidReadQuery("since must be an RFC3339 timestamp within the last 30 days")
		}
		cmd.name = "journalctl"
		cmd.args = []string{"--user", "--unit", p.Unit, "--since", since, "--lines", strconv.Itoa(lines), "--output=short-iso", "--no-pager", "--quiet"}
	case readQueryTmuxSessions:
		cmd.name = "tmux"
		cmd.args = []string{"-N", "list-sessions", "-F", "#{session_id}\\t#{session_name}\\t#{session_windows}\\t#{session_attached}"}
	case readQueryTmuxPanes:
		if !readQuerySessionPattern.MatchString(p.Session) {
			return cmd, invalidReadQuery("session is invalid")
		}
		cmd.name = "tmux"
		cmd.args = []string{"-N", "list-panes", "-s", "-t", p.Session, "-F", "#{pane_id}\\t#{window_name}\\t#{pane_current_command}\\t#{pane_current_path}"}
	case readQueryTmuxCapture:
		if !readQueryPanePattern.MatchString(p.PaneID) {
			return cmd, invalidReadQuery("pane_id must be a tmux pane ID")
		}
		lines, err := boundedInteger(p.Lines, readQueryDefaultLines, readQueryMaxLines)
		if err != nil {
			return cmd, invalidReadQuery("lines must be between 1 and 500")
		}
		cmd.name = "tmux"
		cmd.args = []string{"-N", "capture-pane", "-p", "-J", "-t", p.PaneID, "-S", "-" + strconv.Itoa(lines)}
	default:
		return cmd, invalidReadQuery("operation is not supported")
	}
	return cmd, nil
}

func boundedInteger(value, defaultValue, maxValue int) (int, error) {
	if value == 0 {
		return defaultValue, nil
	}
	if value < 1 || value > maxValue {
		return 0, fmt.Errorf("value must be between 1 and %d", maxValue)
	}
	return value, nil
}

func boundedDuration(value string, defaultValue, maxValue time.Duration) (string, error) {
	if value == "" {
		return defaultValue.String(), nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 || d > maxValue {
		return "", errors.New("duration is out of range")
	}
	return d.String(), nil
}

func boundedJournalSince(value string) (string, error) {
	if value == "" {
		return time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil || t.After(time.Now()) || time.Since(t) > 30*24*time.Hour {
		return "", errors.New("timestamp is out of range")
	}
	return t.UTC().Format(time.RFC3339), nil
}

func validateReadQueryDirectory(path string) (string, error) {
	if path == "" || len(path) > readQueryMaxPathLength || strings.ContainsRune(path, 0) {
		return "", errors.New("invalid directory")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return abs, nil
}

func validatePathspec(pathspec string) error {
	if len(pathspec) > readQueryMaxPathLength || strings.ContainsRune(pathspec, 0) {
		return errors.New("invalid pathspec")
	}
	return nil
}

type limitedOutput struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buffer.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.buffer.Write(p)
}

func runReadQueryCommand(parent context.Context, query readQueryCommand) (ReadQueryResult, *proto.RPCError) {
	ctx, cancel := context.WithTimeout(parent, readQueryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, query.name, query.args...)
	if query.dir != "" {
		cmd.Dir = query.dir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	stdout := &limitedOutput{limit: proto.MaxCommandOutputBytes}
	stderr := &limitedOutput{limit: proto.MaxCommandOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	result := ReadQueryResult{
		Stdout: stdout.buffer.String(), Stderr: stderr.buffer.String(),
		StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated,
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.TimedOut = true
			return result, nil
		}
		return result, &proto.RPCError{Code: proto.ErrTimeout, Message: "read query was canceled"}
	}
	if err == nil {
		code := 0
		result.ExitCode = &code
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		result.ExitCode = &code
		return result, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return ReadQueryResult{}, &proto.RPCError{Code: proto.ErrUnsupported, Message: "required host utility is unavailable"}
	}
	return ReadQueryResult{}, &proto.RPCError{Code: proto.ErrInternal, Message: "read query could not be started"}
}

func (h *ReadQueryHandlers) queryGitHub(ctx context.Context, p ReadQueryParams) (any, *proto.RPCError) {
	if !readQueryOwnerPattern.MatchString(p.Owner) || !readQueryRepoPattern.MatchString(p.Repository) || p.Repository == "." || p.Repository == ".." {
		return nil, invalidReadQuery("owner or repository is invalid")
	}
	path := "/repos/" + url.PathEscape(p.Owner) + "/" + url.PathEscape(p.Repository)
	switch p.Operation {
	case readQueryGitHubRepo:
	case readQueryGitHubIssue, readQueryGitHubPull:
		if p.Number < 1 {
			return nil, invalidReadQuery("number must be positive")
		}
		resource := "issues"
		if p.Operation == readQueryGitHubPull {
			resource = "pulls"
		}
		path += "/" + resource + "/" + strconv.Itoa(p.Number)
	default:
		return nil, invalidReadQuery("operation is not supported")
	}
	base, err := url.Parse(h.apiBase)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: "GitHub API is unavailable"}
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: "GitHub API request could not be created"}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", "command-relay-mcp")
	if token := firstNonEmpty(os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: "GitHub API request failed"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: fmt.Sprintf("GitHub API returned HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, proto.MaxCommandOutputBytes+1))
	if err != nil || len(body) > proto.MaxCommandOutputBytes {
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: "GitHub API response exceeds the output limit or could not be read"}
	}
	var output any
	switch p.Operation {
	case readQueryGitHubRepo:
		var result GitHubRepositoryResult
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInternal, Message: "GitHub API returned an invalid repository response"}
		}
		output = result
	case readQueryGitHubIssue:
		var result GitHubIssueResult
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInternal, Message: "GitHub API returned an invalid issue response"}
		}
		output = result
	case readQueryGitHubPull:
		var result GitHubPullRequestResult
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, &proto.RPCError{Code: proto.ErrInternal, Message: "GitHub API returned an invalid pull request response"}
		}
		output = result
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: "GitHub API response could not be encoded"}
	}
	code := 0
	return ReadQueryResult{Stdout: string(encoded), ExitCode: &code}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type GitHubRepositoryResult struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
}

type GitHubIssueResult struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

type GitHubPullRequestResult struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"`
	HTMLURL   string `json:"html_url"`
	Draft     bool   `json:"draft"`
	Merged    bool   `json:"merged"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}
