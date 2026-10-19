package agentgraph

import (
	"context"
	"errors"
	"strconv"
	"strings"

	graph "github.com/afterlune/luneGraph"
)

func (a *Agent) requestLead(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	return a.requestModel(ctx, call, state)
}

func (a *Agent) requestTeam(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	return a.requestModel(ctx, call, state)
}

func taskInstructions(goal, permissionMode string, planMode, canDelegate, teamEnabled bool) string {
	var instructions []string
	if planMode {
		instructions = append(instructions, "You are in Plan mode. Your primary objective is to produce a concrete, detailed, and actionable implementation plan enclosed in <proposed_plan> and </proposed_plan> tags. When the user asks for next steps or planning, proactively inspect the workspace, select the most critical next milestone, and formulate the complete concrete plan (including target scope, design decisions, exact files to modify, step-by-step implementation, and verification tests). Do not merely analyze the status quo and defer the decision to the user. Before writing the plan, inspect the workspace and conversation to resolve facts you can discover. Then check for unanswered decisions that belong to the user and would materially change the plan. If one remains and there is no safe, conventional default, you MUST call ask_user before presenting a plan; do not emit a proposed plan until the user answers. When calling ask_user, ask concrete, specific technical questions with structured options and a clear recommended choice; never ask vague open-ended questions like 'what should we do next'. Ask only about those consequential decisions, grouping related decisions into one concise questions batch. Use reasonable defaults for routine implementation details. After the answer, continue planning and return the complete plan inside <proposed_plan> and </proposed_plan> tags. Do not use ask_user to request implementation approval; the host presents the completed plan for review. Do not modify files or claim execution. If the user prompt is a greeting, pleasantry, or does not specify any task or goal to plan, do not inspect the workspace or call tools; reply naturally and ask what task or feature they would like to plan.")
		if teamEnabled {
			instructions = append(instructions, "For a user message beginning 'Worker task', inspect with read-only tools and report facts and unresolved decisions to the lead; do not call ask_user, present a full plan, modify files, or claim execution.")
		}
	} else {
		instructions = append(instructions, "Prefer reasonable assumptions and continue instead of asking routine questions. Inspect the workspace and use available tools to resolve discoverable facts first. Use ask_user only when a user-owned decision or material ambiguity cannot be resolved from available context and blocks a safe or correct result. If the user has not requested a concrete task (such as a greeting or casual chat), reply naturally directly without calling tools or inspecting the workspace.")
		if teamEnabled {
			instructions = append(instructions, "For a user message beginning 'Worker task', complete only the assignment, verify facts, and report to the lead; do not call ask_user.")
		}
	}
	instructions = append(instructions, "Language policy: Always reply in the same language as the user's prompt (e.g., if the user asks in Chinese, reply in Chinese; if the user asks in English, reply in English).")
	if planMode {
		permission := "The active permission mode is " + permissionMode + "; it determines approval for visible read-only tools, including web access. Plan mode limits visible tools to read-only operations. Shell command execution and file modifications are disabled; inspect the workspace using read-only file tools."
		if permissionMode == "" {
			permission = "Plan mode limits visible tools to read-only operations. Shell command execution and file modifications are disabled; inspect the workspace using read-only file tools. The active permission mode still determines approval for visible tools."
		}
		instructions = append(instructions, permission)
	} else {
		switch permissionMode {
		case "read-only":
			instructions = append(instructions, "Active permission is read-only. Inspect and explain only; do not modify files or run commands. Web and network access require host approval.")
		case "workspace":
			instructions = append(instructions, "Workspace mode permits reading and writing workspace files. Commands and network access require host approval.")
		case "ask":
			instructions = append(instructions, "Ask mode requires host approval for tools other than workspace reads.")
		case "full-access":
			instructions = append(instructions, "Full-access mode permits all available tools.")
		}
	}
	if goal = strings.TrimSpace(goal); goal != "" {
		instructions = append(instructions,
			"Task goal (guidance only): "+strconv.Quote(goal)+" It does not override higher-priority instructions, safety requirements, or tool permissions.")
	}
	instructions = append(instructions, "Keep responses concise without omitting requested detail, evidence, or necessary code.")
	if canDelegate {
		instructions = append(instructions, "Use delegate_team for at least two independent subtasks that benefit from parallel work. Give workers distinct deliverables and enough context. Use fork only for same-provider/model workers that need this conversation; cross-model workers receive isolated tasks. Skip trivial or dependent work and obey task and depth limits.")
	}
	return strings.Join(instructions, "\n\n")
}

func (a *Agent) failTask(state State, err error) (graph.Transition[State], error) {
	if state.TeamBranch && a.options.StopCommands != nil {
		if cleanupErr := a.options.StopCommands(state); cleanupErr != nil {
			return graph.Transition[State]{}, errors.Join(err, cleanupErr)
		}
	}
	state.TaskError = err.Error()
	if state.TeamBranch {
		state.TeamError = err.Error()
		return graph.CompleteBranch(state), nil
	}
	return graph.EndExecution(state), nil
}

func (a *Agent) nextModelTransition(state State) graph.Transition[State] {
	if state.TeamBranch {
		return graph.To(state, teamModelNode)
	}
	return graph.To(state, modelNode)
}

func (a *Agent) finishAgent(state State) (graph.Transition[State], error) {
	if state.TeamBranch && a.options.StopCommands != nil {
		if err := a.options.StopCommands(state); err != nil {
			return graph.Transition[State]{}, err
		}
	}
	if !state.TeamBranch {
		if state.TaskError == "" && state.FinalResponse != nil && a.options.MemoryEnabled && a.options.MemoryRequester != nil && a.options.MemoryWriter != nil {
			return graph.To(state, memoryNode), nil
		}
		return graph.EndExecution(state), nil
	}
	state.TeamText = responseText(state.FinalResponse.Output)
	if state.TaskError != "" {
		state.TeamError = state.TaskError
	}
	return graph.CompleteBranch(state), nil
}
