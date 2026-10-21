package server

import "time"

// nowMillis is the current time in epoch milliseconds, the unit used by every
// timestamp in the portal.
func nowMillis() int64 {
	return time.Now().UnixMilli()
}