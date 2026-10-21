package tool

import (
	"github.com/afterlune/luneCore/internal/model"
)

// NewDefaultRegistry creates the built-in workspace and web tools. Call Close
// when the registry is no longer in use to release its pinned workspace root.
func NewDefaultRegistry(workspace string, options ...RegistryOptions) (*Registry, error) {
	var registryOptions RegistryOptions
	if len(options) > 0 {
		registryOptions = options[0]
	}
	rootHandle, err := openWorkspaceRoot(workspace)
	if err != nil {
		return nil, err
	}
	workspacePath := rootHandle.Name()
	workspaceID := &workspaceIdentity{root: rootHandle}
	filesCategory := model.ToolCategory{Name: "files", Description: "Read, search, and modify workspace files."}
	commandCategory := model.ToolCategory{Name: "command", Description: "Run workspace commands."}
	webCategory := model.ToolCategory{Name: "web", Description: "Search the web and fetch pages."}
	workspaceRead := model.ToolMetadata{Category: filesCategory, PermissionClass: model.ToolPermissionWorkspaceRead, ReadOnly: true, AlwaysVisible: true}
	workspaceWrite := model.ToolMetadata{Category: filesCategory, PermissionClass: model.ToolPermissionWorkspaceWrite, AlwaysVisible: true}
	command := model.ToolMetadata{Category: commandCategory, PermissionClass: model.ToolPermissionCommand, AlwaysVisible: true}
	network := model.ToolMetadata{Category: webCategory, PermissionClass: model.ToolPermissionNetwork, ReadOnly: true}
	registry, err := newRegistry(registryOptions.Logger,
		ToolRegistration{Tool: ListDirTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceRead},
		ToolRegistration{Tool: GlobTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceRead},
		ToolRegistration{Tool: GrepTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceRead},
		ToolRegistration{Tool: ReadFileTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceRead},
		ToolRegistration{Tool: WriteFileTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceWrite},
		ToolRegistration{Tool: EditFileTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceWrite},
		ToolRegistration{Tool: CommandTool{Workspace: workspacePath, workspace: workspaceID, Executor: registryOptions.CommandExecutor, SandboxHelperPath: registryOptions.SandboxHelperPath, CommandToolchainPaths: append([]string(nil), registryOptions.CommandToolchainPaths...), AllowLocalCommand: registryOptions.AllowLocalCommands}, Metadata: command},
		ToolRegistration{Tool: WebSearchTool{httpClient: defaultWebHTTPClient}, Metadata: network},
		ToolRegistration{Tool: WebFetchTool{httpClient: defaultWebHTTPClient}, Metadata: network},
	)
	if err != nil {
		_ = workspaceID.closeRoot()
		return nil, err
	}
	for _, registration := range registryOptions.Tools {
		if err := registry.Register(registration); err != nil {
			_ = registry.Close()
			return nil, err
		}
	}
	registry.workspaceRoot = workspaceID
	if err := registry.SetPermissionMode(PermissionModeWorkspace); err != nil {
		_ = registry.Close()
		return nil, err
	}
	return registry, nil
}
