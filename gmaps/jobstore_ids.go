package gmaps

import (
	"fmt"
	"time"
)

func newJobID(now time.Time) string {
	return fmt.Sprintf("job_%s_%09d", now.Format("20060102_150405"), now.Nanosecond())
}

func newStrategyID(now time.Time) string {
	return fmt.Sprintf("str_%s_%09d", now.Format("20060102_150405"), now.Nanosecond())
}

func NewStrategyRunID(now time.Time) string {
	return fmt.Sprintf("run_%s_%09d", now.Format("20060102_150405"), now.Nanosecond())
}
