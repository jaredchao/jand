//go:build !darwin && !linux && !freebsd

package taskcontrol

import "errors"

func lock(string) (func(), error) {
	return nil, errors.New("task controller OS locking unsupported on this platform; manual mode required")
}

func Supported() bool { return false }
