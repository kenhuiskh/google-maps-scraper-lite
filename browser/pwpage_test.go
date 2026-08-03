package browser

import (
	"errors"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper-lite/gmaps"
)

func TestPWPageClickForceHardCeiling(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	p := &pwPage{
		clickFn: func(string, time.Duration, time.Duration) error {
			close(started)
			<-release
			return nil
		},
	}

	done := make(chan error, 1)
	startedAt := time.Now()
	go func() {
		done <- p.ClickForce("#review-tab", 5*time.Millisecond, 5*time.Millisecond)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("click driver seam did not start")
	}

	var err error
	select {
	case err = <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("ClickForce did not return at its hard ceiling")
	}
	if elapsed := time.Since(startedAt); elapsed < 1800*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("ClickForce returned after %s, want roughly the 2s hard ceiling", elapsed)
	}
	if !errors.Is(err, gmaps.ErrClickHardTimeout) {
		t.Fatalf("ClickForce error = %v, want ErrClickHardTimeout", err)
	}
	var timeoutErr *gmaps.ClickHardTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("ClickForce error = %T, want ClickHardTimeoutError", err)
	}
	if timeoutErr.Selector != "#review-tab" || timeoutErr.Ceiling != 2*time.Second+10*time.Millisecond {
		t.Fatalf("ClickHardTimeoutError = %+v", timeoutErr)
	}
}

func TestPWPageClickForcePassesThroughDriverResult(t *testing.T) {
	wantErr := errors.New("driver error")
	p := &pwPage{clickFn: func(string, time.Duration, time.Duration) error { return wantErr }}
	if got := p.ClickForce("#review-tab", time.Millisecond, time.Millisecond); got != wantErr {
		t.Fatalf("ClickForce error = %v, want original error %v", got, wantErr)
	}

	p.clickFn = func(string, time.Duration, time.Duration) error { return nil }
	if err := p.ClickForce("#review-tab", time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("ClickForce success returned error: %v", err)
	}
}
