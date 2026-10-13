//go:build linux && (amd64 || arm64)

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	publictool "github.com/afterlune/luneCore/tool"
	"golang.org/x/sys/unix"
)

const (
	landlockCreateRulesetVersion = 1
	landlockRulePathBeneath      = 1
)

func runPlatform(_ context.Context, request publictool.CommandRequest, _ io.Writer) error {
	workspace, err := filepath.EvalSymlinks(request.Workspace)
	if err != nil {
		return fmt.Errorf("resolve sandbox workspace: %w", err)
	}
	if err := os.Chdir(workspace); err != nil {
		return fmt.Errorf("enter sandbox workspace: %w", err)
	}
	if request.Sandbox.TempDir == "" {
		return errors.New("sandbox temporary directory is required")
	}
	tempDir, err := filepath.EvalSymlinks(request.Sandbox.TempDir)
	if err != nil {
		return fmt.Errorf("resolve sandbox temporary directory: %w", err)
	}
	homeDir := tempDir
	if request.Sandbox.PermissionMode == "full-access" && request.Sandbox.HomeDir != "" {
		homeDir, err = filepath.EvalSymlinks(request.Sandbox.HomeDir)
		if err != nil {
			return fmt.Errorf("resolve full-access home directory: %w", err)
		}
	}
	if err := applyLandlock(request.Sandbox, workspace, tempDir); err != nil {
		return fmt.Errorf("install Landlock policy: %w", err)
	}
	if err := installSeccomp(request.Sandbox.Network); err != nil {
		return fmt.Errorf("install seccomp policy: %w", err)
	}
	if err := unix.Exec("/bin/sh", []string{"/bin/sh", "-c", request.Command}, sandboxEnvironment(tempDir, homeDir)); err != nil {
		return fmt.Errorf("start sandbox command: %w", err)
	}
	return nil
}

func applyLandlock(policy publictool.SandboxPolicy, workspace, tempDir string) error {
	if policy.PermissionMode == "full-access" && os.Geteuid() == 0 {
		return errors.New("full-access commands cannot run from a root-owned Core process")
	}
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, landlockCreateRulesetVersion)
	if errno != 0 {
		return fmt.Errorf("Landlock is unavailable: %w", errno)
	}
	if abi < 1 {
		return errors.New("Landlock ABI 1 or later is required")
	}

	handled := landlockFilesystemRights(abi)
	attr := unix.LandlockRulesetAttr{Handled_access_fs: handled}
	ruleset, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr.Handled_access_fs), 0)
	if errno != 0 {
		return fmt.Errorf("create Landlock ruleset: %w", errno)
	}
	defer unix.Close(int(ruleset))

	readOnly := landlockReadOnlyRights()
	readWrite := handled
	paths := make(map[string]uint64)
	if policy.PermissionMode == "full-access" {
		paths["/"] = readWrite
	} else if policy.PermissionMode == "workspace" {
		paths[workspace] = readWrite
		paths[tempDir] = readWrite
		for _, path := range linuxRuntimeReadPaths() {
			paths[path] = readOnly
		}
		paths["/dev/null"] = readWrite
	} else {
		return fmt.Errorf("unsupported sandbox permission mode %q", policy.PermissionMode)
	}
	for _, grant := range policy.Grants {
		resolved, err := filepath.EvalSymlinks(grant.Path)
		if err != nil {
			return fmt.Errorf("resolve granted path %q: %w", grant.Path, err)
		}
		access := readOnly
		if grant.Access == publictool.PathReadWrite {
			access = readWrite
		} else if grant.Access != publictool.PathRead {
			return fmt.Errorf("unsupported path grant access %q", grant.Access)
		}
		paths[resolved] = access
	}

	for path, access := range paths {
		if err := addLandlockPathRule(int(ruleset), path, access); err != nil {
			return err
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("restrict process with Landlock: %w", errno)
	}
	return nil
}

func landlockFilesystemRights(abi uintptr) uint64 {
	rights := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		rights |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		rights |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		rights |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	if abi >= 9 {
		rights |= unix.LANDLOCK_ACCESS_FS_RESOLVE_UNIX
	}
	return rights
}

func landlockReadOnlyRights() uint64 {
	return unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
}

func addLandlockPathRule(ruleset int, path string, access uint64) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect allowed path %q: %w", path, err)
	}
	if !info.IsDir() {
		access &^= unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
			unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
			unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
			unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
			unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK | unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
			unix.LANDLOCK_ACCESS_FS_REFER
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("open allowed path %q: %w", path, err)
	}
	defer unix.Close(fd)
	attr := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(ruleset), landlockRulePathBeneath, uintptr(unsafe.Pointer(&attr)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("add Landlock rule for %q: %w", path, errno)
	}
	return nil
}

func linuxRuntimeReadPaths() []string {
	return []string{
		"/usr", "/usr/local", "/bin", "/sbin", "/lib", "/lib64",
		"/etc/ld.so.cache", "/etc/passwd", "/etc/group", "/etc/nsswitch.conf",
		"/etc/hosts", "/etc/resolv.conf", "/etc/ssl/certs", "/etc/ca-certificates",
		"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom",
	}
}

func sandboxEnvironment(tempDir, homeDir string) []string {
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	return []string{
		"PATH=" + path, "HOME=" + homeDir, "TMPDIR=" + tempDir, "TEMP=" + tempDir,
		"GOCACHE=" + filepath.Join(tempDir, "go-cache"), "GOMODCACHE=" + filepath.Join(tempDir, "go-mod-cache"),
		"XDG_CACHE_HOME=" + filepath.Join(tempDir, "cache"), "LANG=C.UTF-8",
	}
}

func installSeccomp(network bool) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	blocked := []uint32{
		unix.SYS_PTRACE, unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT,
		unix.SYS_UNSHARE, unix.SYS_SETNS, unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT, unix.SYS_FSOPEN,
		unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_MOUNT_SETATTR, unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN,
		unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD, unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE,
		unix.SYS_DELETE_MODULE, unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_KEYCTL, unix.SYS_ADD_KEY,
		unix.SYS_REQUEST_KEY, unix.SYS_REBOOT, unix.SYS_SWAPON, unix.SYS_SWAPOFF,
		unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
		unix.SYS_PIDFD_GETFD, unix.SYS_CAPSET, unix.SYS_SETUID, unix.SYS_SETREUID, unix.SYS_SETRESUID,
		unix.SYS_SETGID, unix.SYS_SETREGID, unix.SYS_SETRESGID, unix.SYS_SETFSUID, unix.SYS_SETFSGID,
		unix.SYS_SETGROUPS,
	}
	if runtime.GOARCH == "amd64" {
		// x86-only I/O privilege syscalls (iopl=172, ioperm=173).
		blocked = append(blocked, 172, 173)
	}
	if !network {
		blocked = append(blocked, unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_CONNECT, unix.SYS_BIND,
			unix.SYS_LISTEN, unix.SYS_ACCEPT, unix.SYS_ACCEPT4, unix.SYS_SENDTO, unix.SYS_RECVFROM,
			unix.SYS_SENDMSG, unix.SYS_RECVMSG)
	}
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, Jf: 0, K: linuxAuditArch()},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	}
	for _, syscallNumber := range blocked {
		filters = append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: syscallNumber},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	filters = append(filters, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0); err != nil {
		return fmt.Errorf("attach seccomp filter: %w", err)
	}
	runtime.KeepAlive(filters)
	runtime.KeepAlive(program)
	return nil
}

func linuxAuditArch() uint32 {
	if runtime.GOARCH == "arm64" {
		return 0xc00000b7
	}
	return 0xc000003e
}
