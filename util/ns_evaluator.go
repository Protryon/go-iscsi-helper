package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

var _zero uintptr

func ForkAndSwitchToNamespace[T interface{}](ns string, timeout time.Duration, toExecute func() (*T, error)) (*T, error) {
	mountNs, err := unix.BytePtrFromString(filepath.Join(ns, "mnt"))
	if err != nil {
		return nil, err
	}
	netNs, err := unix.BytePtrFromString(filepath.Join(ns, "net"))
	if err != nil {
		return nil, err
	}

	// we use a pipe to communicate. VFork-esque style was attempted, but golang is not compatible with it. C could be used instead with VFork.
	pipes := make([]int, 2)
	err = unix.Pipe(pipes)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to init pipes")
	}
	logId := rand.Int()
	fmt.Println(logId, "entering ns", ns)

	pid, _, err := unix.RawSyscall(unix.SYS_FORK, 0, 0, 0)
	if int(pid) == -1 {
		return nil, errors.Wrapf(err, "failed to fork")
	}

	// if pid > 0, we are the parent process
	if pid != 0 {
		// close the sending end in the parent so we don't hold it open.
		close_raw(pipes[1])

		done := make(chan []byte)

		go func() {
			bufSize := 256
			bufIndex := 0
			buf := make([]byte, bufSize)
			for {
				if bufIndex == bufSize {
					// there is a better way to do this
					buf = append(buf, make([]byte, bufSize)...)
					bufSize *= 2
				}
				read, err := read(pipes[0], buf[bufIndex:])
				if err != nil {
					logrus.Errorf("failed to read from setns pipe: %v", err)
					done <- nil
					return
				}
				bufIndex += read
				if read == 0 || bytes.Contains(buf[:bufIndex], []byte{0}) {
					break
				}
			}
			close_raw(pipes[0])
			done <- buf[:bufIndex]
		}()

		select {
		case buf := <-done:
			// -1 to strip terminating nul
			if buf[len(buf)-1] != 0 {
				return nil, errors.New("invalid termination character, not NUL")
			}
			out := string(buf[:len(buf)-1])

			if strings.HasPrefix(out, "err:") {
				fmt.Println(logId, "error ns", strings.TrimPrefix(out, "err:"))
				return nil, errors.New(strings.TrimPrefix(out, "err:"))
			} else if strings.HasPrefix(out, "ok:") {
				var unmarshalled T
				err := json.Unmarshal([]byte(strings.TrimPrefix(out, "ok:")), &unmarshalled)
				if err != nil {
					return nil, err
				}
				fmt.Println(logId, "exiting ns", unmarshalled)
				return &unmarshalled, nil
			}
			return nil, errors.Errorf("unknown response: %s", out)

		case <-time.After(timeout):
			fmt.Println(logId, "timeout ns")
			err = unix.Kill(int(pid), unix.SIGINT)
			if err != nil {
				logrus.Warnf("failed to kill setns process: %v", err)
			}
			return nil, errors.New("ForkAndSwitchToNamespace timed out")
		}
	}

	// child process executes here

	// due to the golang environment being clobbered by fork, any kind of asynchronous call will segfault.
	// to avoid this, we are using RawSyscall which doesn't notify the golang executor that we are doing a blocking operation.
	// that is why we are not using standard utilities for IO or syscalls.
	// Files *should* be fine as they are always blocking.

	err = switchNs(mountNs, netNs)

	// runtime.LockOSThread()

	var out *T

	fmt.Println(logId, "pre-exec ns", err)
	if err == nil {
		out, err = toExecute()
	}
	fmt.Println(logId, "postexec ns", out, err)

	var msg []byte

	if err != nil {
		// pipes[1]
		msg = []byte(fmt.Sprintf("err:%v", err))
	} else {
		base, err := json.Marshal(out)
		if err != nil {
			msg = []byte(fmt.Sprintf("err:%v", err))
		} else {
			msg = []byte(fmt.Sprintf("ok:%s", base))
		}
	}
	// extra NUL byte to signal end of message.
	msg = append(msg, 0)

	written := 0
	for written < len(msg) {
		newly_written, err := write(pipes[1], msg[written:])
		if err != nil {
			break
		}
		written += newly_written
	}
	close_raw(pipes[1])

	// kill forked process immediately, doing no cleanup.
	unix.RawSyscallNoError(unix.SYS_EXIT_GROUP, 0, 0, 0)

	return nil, nil
}

func read(fd int, p []byte) (n int, err error) {
	var _p0 unsafe.Pointer
	if len(p) > 0 {
		_p0 = unsafe.Pointer(&p[0])
	} else {
		_p0 = unsafe.Pointer(&_zero)
	}
	r0, _, err := unix.RawSyscall(unix.SYS_READ, uintptr(fd), uintptr(_p0), uintptr(len(p)))
	n = int(r0)
	if n >= 0 {
		err = nil
	}
	return
}

func write(fd int, p []byte) (n int, err error) {
	var _p0 unsafe.Pointer
	if len(p) > 0 {
		_p0 = unsafe.Pointer(&p[0])
	} else {
		_p0 = unsafe.Pointer(&_zero)
	}
	r0, _, err := unix.RawSyscall(unix.SYS_WRITE, uintptr(fd), uintptr(_p0), uintptr(len(p)))
	n = int(r0)
	if n >= 0 {
		err = nil
	}
	return
}

func openat(dirfd int, path *byte, flags int, mode uint32) (fd int, err error) {
	r0, _, err := unix.RawSyscall6(unix.SYS_OPENAT, uintptr(dirfd), uintptr(unsafe.Pointer(path)), uintptr(flags), uintptr(mode), 0, 0)
	fd = int(r0)
	if fd >= 0 {
		err = nil
	}
	return
}

func open(path *byte, mode int, perm uint32) (fd int, err error) {
	return openat(unix.AT_FDCWD, path, mode|unix.O_LARGEFILE, perm)
}

func setns(fd int, nstype int) (err error) {
	out, _, err := unix.RawSyscall(unix.SYS_SETNS, uintptr(fd), uintptr(nstype), 0)
	if out == 0 {
		err = nil
	}
	return
}

// linter doesn't like this function being named close, even though it's different from the builtin...
func close_raw(fd int) (err error) {
	out, _, err := unix.RawSyscall(unix.SYS_CLOSE, uintptr(fd), 0, 0)
	if out == 0 {
		err = nil
	}
	return
}

func switchNs(mountNs *byte, netNs *byte) error {
	fmt.Println("switchNs", unix.BytePtrToString(mountNs), unix.BytePtrToString(netNs))

	fd, err := open(netNs, unix.O_RDONLY, 0644)
	// if err == unix.ENOENT {
	// 	return nil
	// }
	if err != nil {
		return errors.Wrapf(err, "failed to open net namespace")
	}
	err = setns(fd, unix.CLONE_NEWNET)
	if err != nil {
		close_raw(fd)
		return errors.Wrapf(err, "failed to setns net namespace")
	}
	err = close_raw(fd)
	if err != nil {
		return errors.Wrapf(err, "failed to close net namespace")
	}

	fd, err = open(mountNs, unix.O_RDONLY, 0644)
	if fd < 0 && err != nil {
		return errors.Wrapf(err, "failed to open mount namespace")
	}
	err = setns(fd, unix.CLONE_NEWNS)
	if err != nil {
		close_raw(fd)
		return errors.Wrapf(err, "failed to setns mount namespace")
	}
	err = close_raw(fd)
	if err != nil {
		return errors.Wrapf(err, "failed to close mount namespace")
	}

	return nil
}
