package tui

import (
	"fmt"
	"os"
	"sync"
)

// debugEnabled controls whether debug logging is enabled.
// Set TUI_DEBUG=1 environment variable to enable.
var (
	debugEnabled     bool
	debugEnabledOnce sync.Once
)

// IsDebugEnabled returns true if TUI_DEBUG environment variable is set.
func IsDebugEnabled() bool {
	debugEnabledOnce.Do(func() {
		debugEnabled = os.Getenv("TUI_DEBUG") == "1"
	})
	return debugEnabled
}

// debugLog prints a debug message if debugging is enabled (internal use).
func debugLog(format string, args ...interface{}) {
	if IsDebugEnabled() {
		fmt.Fprintf(os.Stderr, "[PROGRESS DEBUG] "+format+"\n", args...)
	}
}

// DebugLog prints a debug message if debugging is enabled (exported for use in main).
func DebugLog(format string, args ...interface{}) {
	debugLog(format, args...)
}
