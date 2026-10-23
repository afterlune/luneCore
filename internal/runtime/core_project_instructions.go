package runtime

import (
	"context"
	"encoding/json"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
)

const (
	maxProjectInstructionBytes      = tool.MaxProjectInstructionBytes
	maxProjectInstructionTotalBytes = tool.MaxProjectInstructionTotalBytes
)

func (c *Core) initialProjectInstructions(ctx context.Context) ([]agentgraph.ProjectInstruction, error) {
	files, err := c.registry.LoadProjectInstructions(ctx, nil, []string{"."})
	if err != nil {
		return nil, err
	}
	return toAgentgraphInstructions(files), nil
}

func (c *Core) projectInstructionsForTask(ctx context.Context, state agentgraph.State) ([]agentgraph.ProjectInstruction, error) {
	files, err := c.registry.LoadProjectInstructions(ctx, fromAgentgraphInstructions(state.ProjectInstructions), []string{"."})
	if err != nil {
		return nil, err
	}
	return toAgentgraphInstructions(files), nil
}

func (c *Core) projectInstructionsForTool(ctx context.Context, state agentgraph.State, call model.OutputFunctionCall) ([]agentgraph.ProjectInstruction, error) {
	directories, relevant, err := c.registry.InstructionDirectoriesForCall(ctx, call.Name, json.RawMessage(call.Arguments))
	if err != nil || !relevant {
		return nil, err
	}
	directories = append([]string{"."}, directories...)
	files, err := c.registry.LoadProjectInstructions(ctx, fromAgentgraphInstructions(state.ProjectInstructions), directories)
	if err != nil {
		return nil, err
	}
	return toAgentgraphInstructions(files), nil
}

func toAgentgraphInstructions(files []tool.ProjectInstructionFile) []agentgraph.ProjectInstruction {
	if len(files) == 0 {
		return nil
	}
	result := make([]agentgraph.ProjectInstruction, len(files))
	for i, f := range files {
		result[i] = agentgraph.ProjectInstruction{Path: f.Path, Content: f.Content}
	}
	return result
}

func fromAgentgraphInstructions(instructions []agentgraph.ProjectInstruction) []tool.ProjectInstructionFile {
	if len(instructions) == 0 {
		return nil
	}
	result := make([]tool.ProjectInstructionFile, len(instructions))
	for i, ins := range instructions {
		result[i] = tool.ProjectInstructionFile{Path: ins.Path, Content: ins.Content}
	}
	return result
}

func cleanProjectInstructionPath(value string, allowDot bool) (string, error) {
	return tool.CleanProjectInstructionPath(value, allowDot)
}
