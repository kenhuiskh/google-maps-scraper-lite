package gmaps

import (
	"errors"
	"testing"
	"time"
)

func TestPageDiagnosticsStatePreservesOldestConcurrentOperation(t *testing.T) {
	state := NewPageDiagnosticsState("test")
	finishEvaluate := state.BeginOperation("evaluate", "https://example.test/place")
	time.Sleep(time.Millisecond)
	finishClose := state.BeginOperation("close", "")

	snap := state.Snapshot(false)
	if snap.ActiveOperation != "evaluate" {
		t.Fatalf("active operation = %q, want evaluate", snap.ActiveOperation)
	}
	if snap.TargetURL != "https://example.test/place" || snap.PageID == "" || snap.Engine != "test" {
		t.Fatalf("snapshot identity = %#v", snap)
	}

	finishClose(0, nil)
	finishEvaluate(0, errors.New("context deadline exceeded"))
	state.ObservePage("consent", 1234, " Before you continue ")

	snap = state.Snapshot(true)
	if snap.ActiveOperation != "" || !snap.Closed {
		t.Fatalf("completed snapshot = %#v", snap)
	}
	if snap.LastOperation != "evaluate" || snap.LastError != "context deadline exceeded" {
		t.Fatalf("last operation = %#v", snap)
	}
	if snap.PageClass != "consent" || snap.ContentBytes != 1234 || snap.Title != "Before you continue" {
		t.Fatalf("page observation = %#v", snap)
	}
}

func TestDiagnosticCounterForAttempt(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "none", err: nil, want: ""},
		{name: "bot block", err: ErrBotBlocked, want: "bot_block_events"},
		{name: "watchdog", err: ErrScrapeDeadline, want: "watchdog_timeouts"},
		{name: "page crash", err: errors.New("Page crashed while evaluating"), want: "page_crash_events"},
		{name: "cdp", err: errors.New("CDP Runtime.callFunctionOn timed out"), want: "navigation_cdp_errors"},
		{name: "other", err: errors.New("parse failed"), want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := diagnosticCounterForAttempt(tt.err); got != tt.want {
				t.Fatalf("diagnosticCounterForAttempt(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestClassifyPageMetadata(t *testing.T) {
	tests := []struct {
		name, url, want string
		status          int
		sig             pageSignals
	}{
		{name: "rate limited", status: 429, want: "rate_limited"},
		{name: "sorry", url: "https://google.com/sorry/index", want: "sorry"},
		{name: "consent", sig: pageSignals{Consent: true}, want: "consent"},
		{name: "captcha", sig: pageSignals{Captcha: true}, want: "captcha"},
		{name: "traffic", sig: pageSignals{Unusual: true}, want: "unusual_traffic"},
		{name: "maps by url", url: "https://google.com/maps/place/test", want: "maps"},
		{name: "maps by app state", url: "https://example.test", sig: pageSignals{Maps: true}, want: "maps"},
		{name: "unknown", url: "https://example.test", want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPageMetadata(tt.url, tt.status, tt.sig); got != tt.want {
				t.Fatalf("classifyPageMetadata() = %q, want %q", got, tt.want)
			}
		})
	}
}
