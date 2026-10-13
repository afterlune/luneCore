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

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

const (
	commandSchema    = `{"type":"object","properties":{"command":{"type":"string","description":"Shell command to run in the workspace"}},"required":["command"],"additionalProperties":false}`
	maxCommandOutput = 1 << 20
)

// CommandTool delegates local execution to a kernel-sandboxed executor.
type CommandTool struct {
	Workspace         string
	Timeout           time.Duration
	Executor          publictool.CommandExecutor
	SandboxHelperPath string
	AllowLocalCommand bool
}

var highRiskCommandPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\b[^;&|]*-(?:[a-z]*r[a-z]*|recursive)\b`),
	regexp.MustCompile(`(?i)\b(remove-item|del|rmdir|rd)\b[^;&|]*(?:-recurse|/s\b|--recursive)`),
	regexp.MustCompile(`(?i)\b(find)\b[^;&|]*\s-delete\b`),
	regexp.MustCompile(`(?i)\brsync\b[^;&|]*--delete(?:-before|-during|-after|-excluded)?\b`),
	regexp.MustCompile(`(?i)\bgit\s+clean\b[^;&|]*(?:-[a-z]*f[a-z]*|--force\b)`),
	regexp.MustCompile(`(?i)\b(shred|srm)\b`),
	regexp.MustCompile(`(?i)\b(chmod|chown|chgrp|setfacl|icacls|takeown)\b[^;&|]*(?:\s-R\b|--recursive\b|/T\b|/reset\b)`),
	regexp.MustCompile(`(?i)\b(mkfs(?:\.[a-z0-9]+)?|wipefs|fdisk|sfdisk|parted|diskpart|format-volume|clear-disk|initialize-disk|bcdedit)\b`),
	regexp.MustCompile(`(?i)\bdd\b[^;&|]*\bof=/dev/`),
	regexp.MustCompile(`(?i)\b(set-mppreference|set-executionpolicy|iptables(?:-nft|-legacy)?\s+-F|nft\s+flush\s+ruleset|ufw\s+disable|netsh\s+advfirewall|systemctl\s+(enable|disable|mask)|sc(?:\.exe)?\s+(config|create|delete)|reg\s+(delete|add)|net\s+(user|localgroup)|(?:useradd|userdel|usermod|groupadd|groupdel|passwd|chpasswd)|efibootmgr|grub-install|update-grub)\b`),
	regexp.MustCompile(`(?i)(?:^|\s)(sudo|runas|start-process\s+[^;&|]*-verb\s+runas)(?:\s|$)`),
	regexp.MustCompile(`(?i)(-encodedcommand|\b(?:invoke-expression|iex)\b|\bexec\s*\(|\beval\s*\(|\b(?:curl|wget|iwr|invoke-webrequest)\b[^;&|]*\|\s*(?:sh|bash|powershell|iex))`),
	regexp.MustCompile(`(?i)\b(?:sh|bash|zsh|dash|powershell|pwsh)(?:\.exe)?\b[^;&|]*\s-(?:c|command|encodedcommand)(?:\s|$)`),
	regexp.MustCompile(`(?i)\b(?:python(?:\d+(?:\.\d+)*)?|perl|ruby|node|php)\b[^;&|]*\s-(?:c|e|r)(?:\s|$)`),
}

func (CommandTool) Definition() model.FunctionTool {
	return functionDefinition("command", "Run a shell command in the workspace and return its stdout and stderr.", commandSchema)
}

func (t CommandTool) Run(ctx context.Context, raw json.RawMessage) Result {
	var args struct {
		Command string `json:"command"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid command arguments: %w", err)}
	}
	if strings.TrimSpace(args.Command) == "" {
		return Result{Err: errors.New("command must not be empty")}
	}
	workspace, err := canonicalWorkspace(t.Workspace)
	if err != nil {
		return Result{Err: err}
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	executor := t.Executor
	if executor == nil {
		executor = SandboxedCommandExecutor{HelperPath: t.SandboxHelperPath}
	}
	authorization := publictool.PlanAuthorizationFromContext(ctx)
	if reason := highRiskCommandReason(args.Command); reason != "" && !commandApproved(args.Command, authorization.ApprovedHighRiskCommands) {
		return Result{Err: &publictool.ExecutionBlockerError{Tool: "command", Reason: "high-risk command is not individually approved in the plan: " + reason}}
	}
	mode := PermissionModeFromContext(ctx)
	if mode == "" {
		mode = PermissionModeWorkspace
	}
	sandbox := publictool.SandboxPolicy{
		PermissionMode: string(mode), Workspace: workspace,
		Grants:  append([]publictool.PathGrant(nil), authorization.Paths...),
		Network: mode == PermissionModeFullAccess || authorization.AllowNetwork,
	}
	if mode == PermissionModeFullAccess {
		sandbox.HomeDir, _ = os.UserHomeDir()
	}
	invocationID := publictool.InvocationID(ctx)
	requestContext := publictool.WithInvocationID(commandCtx, invocationID)
	result, err := executor.Execute(requestContext, publictool.CommandRequest{
		Command: args.Command, Workspace: workspace, InvocationID: invocationID, Sandbox: sandbox,
	})
	if commandCtx.Err() != nil {
		return Result{Output: result.Output, Err: fmt.Errorf("command stopped: %w", commandCtx.Err())}
	}
	if err != nil {
		var blocker *publictool.ExecutionBlockerError
		if errors.As(err, &blocker) {
			return Result{Err: blocker}
		}
		return Result{Output: result.Output, Err: fmt.Errorf("run sandboxed command: %w", err)}
	}
	return Result{Output: result.Output}
}

func highRiskCommandReason(command string) string {
	for _, pattern := range highRiskCommandPatterns {
		if pattern.MatchString(command) {
			return "the command may destroy data or modify sensitive system settings"
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
