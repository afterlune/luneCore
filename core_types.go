package core

import (
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/session"
)

const (
	legacyRunID                 = "lune-cli-active"
	DefaultAutoCompactThreshold = projectconfig.DefaultAutoCompactThreshold
	DefaultAttachmentMaxFiles   = projectconfig.DefaultAttachmentMaxFiles
)

// ErrStateDirInUse indicates that another Core instance already owns the state directory.
var ErrStateDirInUse = session.ErrStateDirInUse

const (
	DefaultAttachmentMaxFileBytes  = projectconfig.DefaultAttachmentMaxFileBytes
	DefaultAttachmentMaxTotalBytes = projectconfig.DefaultAttachmentMaxTotalBytes
)

const (
	PermissionReadOnly   PermissionMode = "read-only"
	PermissionWorkspace  PermissionMode = "workspace"
	PermissionAsk        PermissionMode = "ask"
	PermissionFullAccess PermissionMode = "full-access"
)
