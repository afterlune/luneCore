package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	commandruntime "github.com/afterlune/luneCore/internal/command"
	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

const (
	commandSchema    = `{"type":"object","properties":{"command":{"type":"string","description":"Non-interactive shell command to run in the workspace"},"background":{"type":"boolean","description":"Keep a requested long-running service alive across task turns. Keep the service in the foreground; do not use shell detachment such as & or nohup. The session owns and can stop it."},"yield_time_ms":{"type":"integer","minimum":0,"maximum":5000,"description":"Initial wait; defaults to 1000 ms. Query command_status if still running."},"timeout_ms":{"type":"integer","minimum":1,"description":"Optional timeout; bounded by the host command limit. Background commands have no default timeout."}},"required":["command"],"additionalProperties":false}`
	maxCommandOutput = 1 << 20
)

// CommandTool delegates local execution to a kernel-sandboxed executor.
type CommandTool struct {
	Manager               *commandruntime.Manager
	Workspace             string
	workspace             *workspaceIdentity
	Timeout               time.Duration
	Executor              publictool.CommandExecutor
	SandboxHelperPath     string
	CommandToolchainPaths []string
	AllowLocalCommand     bool
}

var highRiskCommandPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\b[^;&|]*-(?:[a-z]*r[a-z]*|recursive)\b`),
	regexp.MustCompile(`(?i)\b(remove-item|del|erase|rmdir|rd)\b[^;&|]*(?:-recurse|/s\b|--recursive)`),
	regexp.MustCompile(`(?i)\b(find)\b[^;&|]*\s-delete\b`),
	regexp.MustCompile(`(?i)\brsync\b[^;&|]*--delete(?:-before|-during|-after|-excluded)?\b`),
	regexp.MustCompile(`(?i)\bgit\s+clean\b[^;&|]*(?:-[a-z]*f[a-z]*|--force\b)`),
	regexp.MustCompile(`(?i)\b(shred|srm)\b`),
	regexp.MustCompile(`(?i)\b(chmod|chown|chgrp|setfacl|icacls|takeown)\b[^;&|]*(?:\s-R\b|--recursive\b|/T\b|/reset\b)`),
	regexp.MustCompile(`(?i)\b(mkfs(?:\.[a-z0-9]+)?|wipefs|fdisk|sfdisk|parted|diskpart|format-volume|clear-disk|initialize-disk|bcdedit)\b`),
	regexp.MustCompile(`(?i)\bdd\b[^;&|]*\bof=/dev/`),
	regexp.MustCompile(`(?i)\b(set-mppreference|set-executionpolicy|iptables(?:-nft|-legacy)?\s+-F|nft\s+flush\s+ruleset|ufw\s+disable|netsh\s+advfirewall|systemctl\s+(enable|disable|mask)|sc(?:\.exe)?\s+(config|create|delete)|reg\s+(delete|add)|net\s+(user|localgroup)|(?:useradd|userdel|usermod|groupadd|groupdel|passwd|chpasswd)|efibootmgr|grub-install|update-grub)\b`),
	regexp.MustCompile(`(?i)(?:^|\s)(sudo|runas|start-process\s+[^;&|]*-verb\s+runas)(?:\s|$)`),
	regexp.MustCompile(`(?i)\bcmd(?:\.exe)?\b[^;&|]*/[ck](?:\s|$|")`),
	regexp.MustCompile(`(?i)(-encodedcommand|\b(?:invoke-expression|iex)\b|\bexec\s*\(|\beval\s*\(|\b(?:curl|wget|iwr|invoke-webrequest)\b[^;&|]*\|\s*(?:sh|bash|powershell|iex))`),
	regexp.MustCompile(`(?i)\b(?:sh|bash|zsh|dash|powershell|pwsh)(?:\.exe)?\b[^;&|]*\s-(?:c|command|encodedcommand)(?:\s|$)`),
	regexp.MustCompile(`(?i)\b(?:python(?:\d+(?:\.\d+)*)?|perl|ruby|node|php)\b[^;&|]*\s-(?:c|e|r)(?:\s|$)`),
}

var extremeDangerCommandPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:mkfs(?:\.[a-z0-9]+)?|wipefs|format-volume|clear-disk|initialize-disk|pvremove|vgremove|lvremove|zpool\s+destroy)\b`),
	regexp.MustCompile(`(?i)(?:^|[;&|]\s*)format(?:\.com)?\s+[^;&|]*(?:\b[a-z]:\\?|/fs:|/q\b)`),
	regexp.MustCompile(`(?i)\b(?:dd|shred|srm)\b[^;&|]*(?:\bof=)?\s*/dev/(?:sd[a-z]+\d*|nvme\d+n\d+(?:p\d+)?|vd[a-z]+\d*|xvd[a-z]+\d*|mmcblk\d+(?:p\d+)?|loop\d+)\b`),
	regexp.MustCompile(`(?i)\b(?:fdisk|sfdisk|parted|diskpart)\b[^;&|]*(?:\b(?:clean|format|delete\s+partition|remove\s+partition|mklabel|mkpart)\b)`),
	regexp.MustCompile(`(?i)\b(?:grub-install|update-grub)\b|\befibootmgr\b[^;&|]*\s-(?:B|b\s+\d+\s+-[Bb])\b|\bbcdedit\b[^;&|]*\s/(?:delete|set|deletevalue|import|store)\b`),
	regexp.MustCompile(`(?i)\b(?:set-mppreference|iptables(?:-nft|-legacy)?\s+-F|nft\s+flush\s+ruleset|ufw\s+disable|netsh\s+advfirewall\s+(?:reset|set\s+[^;&|]*state\s+off)|set-executionpolicy)\b`),
	regexp.MustCompile(`(?i)(?:^|\s)(?:sudo|doas|su|pkexec|runas|start-process\s+[^;&|]*-verb\s+runas)(?:\s|$)`),
	regexp.MustCompile(`(?i)\bsystemctl\s+(?:stop|disable|mask)\s+(?:firewalld|ufw|apparmor|selinux)(?:\s|$)`),
	regexp.MustCompile(`(?i)\b(?:rm|rmdir)\b[^;&|]*-(?:[a-z]*r[a-z]*|recursive)\b[^;&|]*(?:^|\s)(?:/|/(?:etc|boot|usr|bin|sbin|lib|lib64|var|root|proc|sys|dev)(?:/|\s|$))`),
	regexp.MustCompile(`(?i)\b(?:remove-item|del|erase|rmdir|rd)\b[^;&|]*(?:-recurse|/s\b|--recursive)[^;&|]*(?:^|\s)(?:[a-z]:\\(?:\s|$)|[a-z]:\\(?:windows|program files(?: \(x86\))?)(?:\\|\s|$))`),
}

func (CommandTool) Definition() model.FunctionTool {
	return functionDefinition("command", "Run a non-interactive workspace command with bounded output. For a service the user wants to keep running, set background=true and keep the process in the foreground; it remains managed by this session after the task ends. Poll command_status for readiness, or stop it with command_stop.", commandSchema)
}

func (t CommandTool) Run(ctx context.Context, raw json.RawMessage) Result {
	var args struct {
		Command    string `json:"command"`
		Background bool   `json:"background"`
		Yield      *int64 `json:"yield_time_ms"`
		Timeout    *int64 `json:"timeout_ms"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid command arguments: %w", err)}
	}
	if strings.TrimSpace(args.Command) == "" {
		return Result{Err: errors.New("command must not be empty")}
	}
	yield := time.Second
	if args.Yield != nil {
		if *args.Yield < 0 || *args.Yield > 5000 {
			return Result{Err: errors.New("yield_time_ms must be between 0 and 5000")}
		}
		yield = time.Duration(*args.Yield) * time.Millisecond
	}
	timeout := t.Timeout
	if args.Background && args.Timeout == nil {
		// A managed service lives until explicitly stopped or its session ends.
		timeout = 0
	}
	if args.Timeout != nil {
		if *args.Timeout <= 0 || *args.Timeout > int64((time.Duration(1<<63-1))/time.Millisecond) {
			return Result{Err: errors.New("invalid timeout_ms")}
		}
		timeout = time.Duration(*args.Timeout) * time.Millisecond
	}
	workspace := t.Workspace
	if t.workspace != nil && t.workspace.root != nil {
		resolved, err := t.workspace.resolvedPath()
		if err != nil {
			return Result{Err: err}
		}
		workspace = resolved
	} else {
		var err error
		workspace, err = canonicalWorkspace(workspace)
		if err != nil {
			return Result{Err: err}
		}
	}
	mode := PermissionModeFromContext(ctx)
	if mode == "" {
		mode = PermissionModeWorkspace
	}
	authorization := publictool.PlanAuthorizationFromContext(ctx)
	if reason := extremeDangerCommandReason(args.Command); reason != "" {
		return Result{Err: &publictool.ExecutionBlockerError{Tool: "command", Reason: "command is permanently blocked as extremely dangerous: " + reason}}
	}
	if publictool.NoInteractiveDecisions(ctx) && highRiskCommandReason(args.Command) != "" && !commandApproved(args.Command, authorization.ApprovedHighRiskCommands) {
		return Result{Err: &publictool.ExecutionBlockerError{Tool: "command", Reason: "high-risk command is not individually approved in the plan"}}
	}
	sandboxMode := mode
	if sandboxMode == PermissionModeAuto {
		// Auto approval is resolved by the Core permission controller. Once
		// approved, execute with the regular workspace sandbox boundaries.
		sandboxMode = PermissionModeWorkspace
	}
	grants := append([]publictool.PathGrant(nil), authorization.Paths...)
	for _, path := range t.CommandToolchainPaths {
		grants = append(grants, publictool.PathGrant{Path: path, Access: publictool.PathRead})
	}
	sandbox := publictool.SandboxPolicy{
		PermissionMode: string(sandboxMode), Workspace: workspace, Grants: grants,
		CommandToolchainPaths: append([]string(nil), t.CommandToolchainPaths...),
		Network:               true,
	}
	if mode == PermissionModeFullAccess {
		sandbox.HomeDir, _ = os.UserHomeDir()
	}
	request := publictool.CommandRequest{Command: args.Command, Workspace: workspace, InvocationID: publictool.InvocationID(ctx), Background: args.Background, Sandbox: sandbox}
	if t.Manager != nil {
		output, err := t.Manager.Start(ctx, request, timeout, yield)
		if err != nil {
			return Result{Err: err}
		}
		data, err := json.Marshal(output)
		return Result{Output: string(data), Err: err, Details: &publictool.ExecutionDetails{Command: &output}}
	}
	if args.Background {
		return Result{Err: errors.New("managed background commands require an active Core command manager")}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	executor := t.Executor
	if executor == nil {
		if IsSandboxDisabled(t.AllowLocalCommand) {
			executor = DirectCommandExecutor{}
		} else {
			executor = SandboxedCommandExecutor{HelperPath: t.SandboxHelperPath}
		}
	}
	result, err := executor.Execute(commandCtx, request)
	status := publictool.CommandSucceeded
	if err != nil || result.ExitCode != nil && *result.ExitCode != 0 {
		status = publictool.CommandFailed
	}
	details := &publictool.ExecutionDetails{Command: &publictool.CommandOutput{Command: publictool.CommandInfo{Command: args.Command, InvocationID: publictool.InvocationID(ctx), Status: status, ExitCode: result.ExitCode}, Output: result.Output}}
	if commandCtx.Err() != nil {
		return Result{Details: details, Output: result.Output, Err: fmt.Errorf("command stopped: %w", commandCtx.Err())}
	}
	if err != nil {
		var blocker *publictool.ExecutionBlockerError
		if errors.As(err, &blocker) {
			return Result{Err: blocker}
		}
		return Result{Details: details, Output: result.Output, Err: fmt.Errorf("run sandboxed command: %w", err)}
	}
	return Result{Output: result.Output, Details: details}
}

func highRiskCommandReason(command string) string {
	for _, pattern := range highRiskCommandPatterns {
		if pattern.MatchString(command) {
			return "the command may destroy data or modify sensitive system settings"
		}
	}
	return ""
}

func extremeDangerCommandReason(command string) string {
	for _, pattern := range extremeDangerCommandPatterns {
		if pattern.MatchString(command) {
			return "the command can irreversibly damage system storage, startup, security controls, or operating-system directories"
		}
	}
	return ""
}

func commandApproved(command string, approved []string) bool {
	normalized := normalizeCommand(command)
	for _, candidate := range approved {
		if normalizeCommand(candidate) == normalized {
			return true
		}
	}
	return false
}

func normalizeCommand(command string) string {
	return strings.Join(strings.Fields(command), " ")
}

// limitedBuffer captures command output up to a fixed limit.
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLength := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return originalLength, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buf.Write(p)
	return originalLength, nil
}

func (b *limitedBuffer) String() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String(), b.truncated
}
