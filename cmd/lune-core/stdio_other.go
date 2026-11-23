//go:build !unix

package main

import "os"

func runtimePipe(file *os.File) (*os.File, error) {
	return file, nil
}
