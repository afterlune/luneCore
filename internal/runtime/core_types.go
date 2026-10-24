package runtime

import (
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/session"
)

const (
	DefaultAutoCompactThreshold = projectconfig.DefaultAutoCompactThreshold
	DefaultAttachmentMaxFiles   = projectconfig.DefaultAttachmentMaxFiles
)

// ErrSessionInUse indicates that another Core instance already owns a session.
var ErrSessionInUse = session.ErrSessionInUse

const (
	DefaultAttachmentMaxFileBytes  = projectconfig.DefaultAttachmentMaxFileBytes
	DefaultAttachmentMaxTotalBytes = projectconfig.DefaultAttachmentMaxTotalBytes
)

const (
	PermissionReadOnly   PermissionMode = "read-only"
	PermissionWorkspace  PermissionMode = "workspace"
	PermissionAuto       PermissionMode = "auto"
	PermissionFullAccess PermissionMode = "full-access"

	// Deprecated: use PermissionAuto instead.
	PermissionAsk        PermissionMode = "ask"
)
