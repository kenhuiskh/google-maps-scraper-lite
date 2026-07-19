package gmaps

import (
	"log"
	"os"
	"time"
)

var stageTimingsEnabled = os.Getenv("GMAPS_TIMINGS") == "1"

func logStageTiming(stage string, started time.Time) {
	if !stageTimingsEnabled {
		return
	}

	log.Printf(
		"TIMING stage=%s duration=%s",
		stage,
		time.Since(started),
	)
}
