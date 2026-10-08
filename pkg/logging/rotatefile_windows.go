//go:build windows

package logging

// putFileDownBeforeRotate is set on Windows, which refuses to rename a file
// another handle holds open; open() closes the previous day's file first so the
// rotation can move the live name aside.
const putFileDownBeforeRotate = true
