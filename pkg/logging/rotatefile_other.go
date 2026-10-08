//go:build !windows

package logging

// putFileDownBeforeRotate is off where renaming an open file works: the handle
// that stays open follows the archive, and it is what keeps the lines flowing
// when the new day's file cannot be opened (the fallback
// TestAFailedReopenKeepsTheOpenFileForTheNextLine pins down).
const putFileDownBeforeRotate = false
