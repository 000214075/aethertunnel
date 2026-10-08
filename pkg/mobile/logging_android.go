//go:build android && cgo

package mobile

/*
#include <android/log.h>
#include <stdlib.h>
*/
import "C"

import (
	"os"
	"unsafe"
)

// On Android a bind library has no console: os.Stderr goes nowhere, and the
// client's log lines would be invisible to `adb logcat` — and to anyone
// debugging an install. This wires stderr to logcat the way the app package
// does: a pipe drains into __android_log_write, one line per call, under the
// GoLog tag.
//
// The file needs cgo, hence the build tag: plain GOOS=android builds without
// cgo (the default build's) compile without it and keep os.Stderr as is.
func init() {
	r, w, err := os.Pipe()
	if err != nil {
		return
	}
	os.Stderr = w

	go func() {
		defer r.Close()
		buf := make([]byte, 0, 8192)
		chunk := make([]byte, 2048)
		for {
			n, readErr := r.Read(chunk)
			if n > 0 {
				buf = append(buf, chunk[:n]...)
				for {
					nl := -1
					for i, b := range buf {
						if b == '\n' {
							nl = i
							break
						}
					}
					if nl < 0 {
						if len(buf) > 8192 { // flush a pathological line
							logLine(buf)
							buf = buf[:0]
						}
						break
					}
					logLine(buf[:nl])
					buf = buf[nl+1:]
				}
			}
			if readErr != nil {
				// The write end is gone; whatever is still in the buffer is
				// the tail of a line the process wrote without a trailing
				// newline. Dropping it here would lose the last log line
				// exactly when the process is ending and the line is most
				// likely about why.
				if len(buf) > 0 {
					logLine(buf)
				}
				return
			}
		}
	}()
}

func logLine(line []byte) {
	tag := C.CString("GoLog")
	defer C.free(unsafe.Pointer(tag))
	msg := C.CString(string(line))
	defer C.free(unsafe.Pointer(msg))
	C.__android_log_write(C.ANDROID_LOG_INFO, tag, msg)
}
