package worker

import (
	"strings"
	"sync/atomic"
	"time"
)

func rollupWakeAllowed(lastWakeAtMS *int64, minInterval time.Duration) bool {
	if lastWakeAtMS == nil || minInterval <= 0 {
		return true
	}
	nowMS := time.Now().UnixMilli()
	minIntervalMS := minInterval.Milliseconds()
	for {
		last := atomic.LoadInt64(lastWakeAtMS)
		if last > 0 && nowMS-last < minIntervalMS {
			return false
		}
		if atomic.CompareAndSwapInt64(lastWakeAtMS, last, nowMS) {
			return true
		}
	}
}

func isSQLiteBusyError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked") ||
		strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "locked (517)")
}
