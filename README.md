# Lune Core

Lune Core is the Go runtime library used by the Lune front ends. Applications construct `*core.Core` in their own process and use the public API for sessions, tasks, approvals, streaming events, usage, and tools. It does not run a daemon or expose an IPC/network service.

## Install

```sh
go get github.com/afterlune/luneCore@latest
```

## Quick start

```go
package main

import (
	"context"
	"encoding/json"
	"log"

	core "github.com/afterlune/luneCore"
)

func main() {
	ctx := context.Background()
	client, err := core.New(core.Config{Workspace: "."})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	session, found, err := client.LatestSession(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if !found {
		session, err = client.CreateSession(ctx, json.RawMessage("null"))
		if err != nil {
			log.Fatal(err)
		}
	}

	input, err := core.NewTaskInput("Review this workspace.")
	if err != nil {
		log.Fatal(err)
	}
	_, err = client.RunTask(ctx, core.TaskRequest{SessionID: session.ID, Input: input})
	if err != nil {
		log.Fatal(err)
	}
}
```

The workspace supplies `config.json`; Core stores sessions, checkpoints, team runs, and enabled MCP-server state under `.luneagent`. Front ends receive runtime events through `SetEventHandler` and resolve tool approvals or user interactions through the matching Core methods.

Runtime packages remain implementation details inside this module. Consumers should import the root `core` package and use its exported types and methods.