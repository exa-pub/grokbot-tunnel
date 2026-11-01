// Package nodeerr words file system errors as Node.js does, for texts that the
// desktop client passes on as they are: "EACCES: permission denied, open
// '/abs/path'".
package nodeerr

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// libuv's codes and descriptions (uv_err_name, uv_strerror).
var names = map[syscall.Errno][2]string{
	syscall.EACCES:       {"EACCES", "permission denied"},
	syscall.EAGAIN:       {"EAGAIN", "resource temporarily unavailable"},
	syscall.EBADF:        {"EBADF", "bad file descriptor"},
	syscall.EBUSY:        {"EBUSY", "resource busy or locked"},
	syscall.EEXIST:       {"EEXIST", "file already exists"},
	syscall.EFBIG:        {"EFBIG", "file too large"},
	syscall.EINVAL:       {"EINVAL", "invalid argument"},
	syscall.EIO:          {"EIO", "i/o error"},
	syscall.EISDIR:       {"EISDIR", "illegal operation on a directory"},
	syscall.ELOOP:        {"ELOOP", "too many symbolic links encountered"},
	syscall.EMFILE:       {"EMFILE", "too many open files"},
	syscall.ENAMETOOLONG: {"ENAMETOOLONG", "name too long"},
	syscall.ENFILE:       {"ENFILE", "file table overflow"},
	syscall.ENODEV:       {"ENODEV", "no such device"},
	syscall.ENOENT:       {"ENOENT", "no such file or directory"},
	syscall.ENOMEM:       {"ENOMEM", "not enough memory"},
	syscall.ENOSPC:       {"ENOSPC", "no space left on device"},
	syscall.ENOTDIR:      {"ENOTDIR", "not a directory"},
	syscall.ENOTEMPTY:    {"ENOTEMPTY", "directory not empty"},
	syscall.ENXIO:        {"ENXIO", "no such device or address"},
	syscall.EOVERFLOW:    {"EOVERFLOW", "value too large for defined data type"},
	syscall.EPERM:        {"EPERM", "operation not permitted"},
	syscall.EROFS:        {"EROFS", "read-only file system"},
	syscall.ESPIPE:       {"ESPIPE", "invalid seek"},
	syscall.ETXTBSY:      {"ETXTBSY", "text file is busy"},
	syscall.EXDEV:        {"EXDEV", "cross-device link not permitted"},
}

// Error is a file system error in Node's words. It unwraps to the original
// error.
type Error struct {
	Code, Description, Syscall, Path string
	Err                              error
}

func (e *Error) Error() string {
	msg := e.Code + ": " + e.Description + ", " + e.Syscall
	if e.Path != "" {
		msg += " '" + e.Path + "'"
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Wrap words err as Node would for an operation on path: Go's operation name
// becomes Node's syscall (openat → open, statat → stat, readdirent →
// scandir) and the path is shown absolute. Operations on an open file, such
// as read, name no path, as in Node. Errors without a known errno are
// returned as they are.
func Wrap(err error, path string) error {
	var errno syscall.Errno
	if err == nil || !errors.As(err, &errno) {
		return err
	}
	name, ok := names[errno]
	if !ok {
		return err
	}
	op := "open"
	var pathErr *os.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &pathErr):
		op = pathErr.Op
	case errors.As(err, &linkErr):
		op = linkErr.Op
	}
	op = Syscall(op)
	switch op {
	case "read", "write", "fstat", "fsync", "close", "fchmod", "fchown":
		path = ""
	}
	return &Error{Code: name[0], Description: name[1], Syscall: op, Path: path, Err: err}
}

// Syscall converts the name of a Go file operation to Node's.
func Syscall(op string) string {
	switch op {
	case "readdirent", "readdir", "fdopendir":
		return "scandir"
	case "fstatat", "statat":
		return "stat"
	case "lstatat":
		return "lstat"
	case "fchmodat", "chmodat":
		return "chmod"
	case "sync":
		return "fsync"
	case "openat", "mkdirat", "renameat", "unlinkat", "readlinkat", "symlinkat", "linkat", "fchownat":
		return strings.TrimSuffix(op, "at")
	}
	return op
}
