package gateway

import (
	"context"
	"encoding/json"

	"command-relay-mcp/agent"
	"command-relay-mcp/internal/proto"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type readQueryInput struct {
	DeviceID string `json:"device_id"`
}

type gitRepositoryInput struct {
	DeviceID       string `json:"device_id"`
	RepositoryPath string `json:"repository_path"`
}

type gitDiffInput struct {
	DeviceID       string `json:"device_id"`
	RepositoryPath string `json:"repository_path"`
	Pathspec       string `json:"pathspec,omitempty"`
	Staged         bool   `json:"staged,omitempty"`
}

type gitLogInput struct {
	DeviceID       string `json:"device_id"`
	RepositoryPath string `json:"repository_path"`
	Pathspec       string `json:"pathspec,omitempty"`
	Limit          int    `json:"limit,omitempty"`
}

type dockerPSInput struct {
	DeviceID   string `json:"device_id"`
	IncludeAll bool   `json:"include_all,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

type dockerContainerInput struct {
	DeviceID  string `json:"device_id"`
	Container string `json:"container"`
}

type dockerLogsInput struct {
	DeviceID  string `json:"device_id"`
	Container string `json:"container"`
	Tail      int    `json:"tail,omitempty"`
	Since     string `json:"since,omitempty"`
}

type systemdUnitInput struct {
	DeviceID string `json:"device_id"`
	Unit     string `json:"unit"`
}

type journalQueryInput struct {
	DeviceID string `json:"device_id"`
	Unit     string `json:"unit"`
	Since    string `json:"since,omitempty"`
	Lines    int    `json:"lines,omitempty"`
}

type tmuxSessionInput struct {
	DeviceID string `json:"device_id"`
	Session  string `json:"session"`
}

type tmuxCaptureInput struct {
	DeviceID string `json:"device_id"`
	PaneID   string `json:"pane_id"`
	Lines    int    `json:"lines,omitempty"`
}

type githubRepositoryInput struct {
	DeviceID   string `json:"device_id"`
	Owner      string `json:"owner"`
	Repository string `json:"repository"`
}

type githubNumberInput struct {
	DeviceID   string `json:"device_id"`
	Owner      string `json:"owner"`
	Repository string `json:"repository"`
	Number     int    `json:"number"`
}

func registerReadQueryTools(server *mcp.Server, reg *Registry) {
	addReadQueryTool(server, reg, "git_status", "Read the status of a Git working tree without refreshing its index.", func(in gitRepositoryInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "git_status", RepositoryPath: in.RepositoryPath}
	})
	addReadQueryTool(server, reg, "git_diff", "Read staged or unstaged Git differences, optionally limited to one pathspec.", func(in gitDiffInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "git_diff", RepositoryPath: in.RepositoryPath, Pathspec: in.Pathspec, Staged: in.Staged}
	})
	addReadQueryTool(server, reg, "git_log", "Read up to 200 Git commits (default 20), optionally limited to one pathspec.", func(in gitLogInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "git_log", RepositoryPath: in.RepositoryPath, Pathspec: in.Pathspec, Limit: in.Limit}
	})
	addReadQueryTool(server, reg, "github_repo", "Read public or authorized private repository metadata from github.com. Uses GH_TOKEN or GITHUB_TOKEN when set.", func(in githubRepositoryInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "github_repo", Owner: in.Owner, Repository: in.Repository}
	})
	addReadQueryTool(server, reg, "github_issue", "Read one GitHub issue by repository and issue number. Uses GH_TOKEN or GITHUB_TOKEN when set.", func(in githubNumberInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "github_issue", Owner: in.Owner, Repository: in.Repository, Number: in.Number}
	})
	addReadQueryTool(server, reg, "github_pull_request", "Read one GitHub pull request by repository and number. Uses GH_TOKEN or GITHUB_TOKEN when set.", func(in githubNumberInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "github_pull_request", Owner: in.Owner, Repository: in.Repository, Number: in.Number}
	})
	addReadQueryTool(server, reg, "docker_ps", "List up to 500 Docker containers (default 100) without command, label, or environment details.", func(in dockerPSInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "docker_ps", IncludeAll: in.IncludeAll, Limit: in.Limit}
	})
	addReadQueryTool(server, reg, "docker_inspect", "Read selected status and image fields for one Docker container.", func(in dockerContainerInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "docker_inspect", Container: in.Container}
	})
	addReadQueryTool(server, reg, "docker_logs", "Read up to 500 log lines (default 100) from a container, with since as a Go duration no longer than 168h (default 1h).", func(in dockerLogsInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "docker_logs", Container: in.Container, Tail: in.Tail, Since: in.Since}
	})
	addReadQueryTool(server, reg, "systemd_status", "Read selected status fields for one user systemd unit.", func(in systemdUnitInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "systemd_status", Unit: in.Unit}
	})
	addReadQueryTool(server, reg, "systemd_units", "List user systemd service units.", func(in readQueryInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "systemd_units"}
	})
	addReadQueryTool(server, reg, "journal_query", "Read up to 500 lines (default 100) for one user systemd unit from an RFC3339 timestamp within the last 30 days (default 1h ago).", func(in journalQueryInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "journal_query", Unit: in.Unit, Since: in.Since, Lines: in.Lines}
	})
	addReadQueryTool(server, reg, "tmux_sessions", "List existing tmux sessions without starting a server.", func(in readQueryInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "tmux_sessions"}
	})
	addReadQueryTool(server, reg, "tmux_panes", "List panes in one existing tmux session.", func(in tmuxSessionInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "tmux_panes", Session: in.Session}
	})
	addReadQueryTool(server, reg, "tmux_capture", "Read up to 500 recent lines (default 100) from one tmux pane ID.", func(in tmuxCaptureInput) agent.ReadQueryParams {
		return agent.ReadQueryParams{Operation: "tmux_capture", PaneID: in.PaneID, Lines: in.Lines}
	})
}

func addReadQueryTool[In any](server *mcp.Server, reg *Registry, name, description string, toParams func(In) agent.ReadQueryParams) {
	mcp.AddTool(server, &mcp.Tool{
		Name: name, Description: description,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, agent.ReadQueryResult, error) {
		params := toParams(in)
		deviceID := readQueryDeviceID(in)
		raw, err := deviceCall(ctx, reg, deviceID, proto.MethodReadQuery, params)
		if err != nil {
			return nil, agent.ReadQueryResult{}, err
		}
		var result agent.ReadQueryResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, agent.ReadQueryResult{}, err
		}
		return nil, result, nil
	})
}

// readQueryDeviceID extracts the common routing field from the concrete tool
// input without exposing a caller-selectable operation or command string.
func readQueryDeviceID(value any) string {
	switch in := value.(type) {
	case readQueryInput:
		return in.DeviceID
	case gitRepositoryInput:
		return in.DeviceID
	case gitDiffInput:
		return in.DeviceID
	case gitLogInput:
		return in.DeviceID
	case dockerPSInput:
		return in.DeviceID
	case dockerContainerInput:
		return in.DeviceID
	case dockerLogsInput:
		return in.DeviceID
	case systemdUnitInput:
		return in.DeviceID
	case journalQueryInput:
		return in.DeviceID
	case tmuxSessionInput:
		return in.DeviceID
	case tmuxCaptureInput:
		return in.DeviceID
	case githubRepositoryInput:
		return in.DeviceID
	case githubNumberInput:
		return in.DeviceID
	default:
		return ""
	}
}
