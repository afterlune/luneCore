//go:build !unix

package rpcserver

import "os"

func openWorkspacePreview(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
