package tool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func replaceFile(ctx context.Context, root *os.Root, relative string, mode fs.FileMode, preserveMode bool, write func(io.Writer) error) error {
	tempMode := mode.Perm()
	if preserveMode {
		tempMode = 0o600
	}
	tempName, file, err := createWorkspaceTemp(root, filepath.Dir(relative), tempMode)
	if err != nil {
		return fmt.Errorf("create temporary file for %q: %w", relative, err)
	}
	committed := false
	defer func() {
		if file != nil {
			_ = file.Close()
		}
		if !committed {
			_ = root.Remove(tempName)
		}
	}()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("write stopped: %w", err)
	}
	if err := write(file); err != nil {
		return err
	}
	if preserveMode {
		if err := file.Chmod(mode.Perm()); err != nil {
			return fmt.Errorf("preserve permissions for %q: %w", relative, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("write stopped: %w", err)
	}
	if err := file.Close(); err != nil {
		file = nil
		return fmt.Errorf("close temporary file for %q: %w", relative, err)
	}
	file = nil
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("write stopped: %w", err)
	}
	if err := root.Rename(tempName, relative); err != nil {
		return fmt.Errorf("replace %q: %w", relative, err)
	}
	committed = true
	return nil
}

func createWorkspaceTemp(root *os.Root, directory string, mode fs.FileMode) (string, *os.File, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := filepath.Join(directory, ".lune-tmp-"+hex.EncodeToString(random[:]))
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
		if err == nil {
			return name, file, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("could not allocate a unique temporary name")
}

func writeContextString(ctx context.Context, destination io.Writer, value string) error {
	const chunkSize = 64 * 1024
	for len(value) > 0 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("write stopped: %w", err)
		}
		chunk := value
		if len(chunk) > chunkSize {
			chunk = chunk[:chunkSize]
		}
		written, err := io.WriteString(destination, chunk)
		if err != nil {
			return fmt.Errorf("write temporary file: %w", err)
		}
		if written != len(chunk) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func writeContextBytes(ctx context.Context, destination io.Writer, value []byte) error {
	const chunkSize = 64 * 1024
	for len(value) > 0 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("write stopped: %w", err)
		}
		chunk := value
		if len(chunk) > chunkSize {
			chunk = chunk[:chunkSize]
		}
		if err := writeAll(destination, chunk); err != nil {
			return fmt.Errorf("write temporary file: %w", err)
		}
		value = value[len(chunk):]
	}
	return nil
}

func writeAll(destination io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := destination.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
