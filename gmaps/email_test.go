package gmaps

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withMockDNS(t *testing.T, fn func(host string) ([]net.IP, error)) {
	t.Helper()
	prev := emailLookupHost
	emailLookupHost = fn
	t.Cleanup(func() { emailLookupHost = prev })
}

func TestValidateExternalURL_BlocksLiterals(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/",
		"http://10.0.0.5/",
		"http://192.168.1.1/",
		"http://169.254.169.254/",
		"http://100.64.0.1/",
		"http://[::1]/",
		"http://[fe80::1]/",
		"http://[fc00::1]/",
		"http://0.0.0.0/",
		"ftp://example.com/",
		"file:///etc/passwd",
		"http:///",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := validateExternalURL(raw); err == nil {
				t.Fatalf("expected error for %q", raw)
			}
		})
	}
}

func TestValidateExternalURL_BlocksPrivateDNS(t *testing.T) {
	withMockDNS(t, func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.1.2.3")}, nil
	})
	if _, err := validateExternalURL("http://internal.example/"); !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("expected ErrBlockedTarget, got %v", err)
	}
}

func TestValidateExternalURL_AllowsPublic(t *testing.T) {
	withMockDNS(t, func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})
	if _, err := validateExternalURL("http://example.com/"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExtractEmails_RejectsLocalhost(t *testing.T) {
	_, err := ExtractEmails(context.Background(), "http://127.0.0.1/")
	if !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("expected ErrBlockedTarget, got %v", err)
	}
}

func TestExtractEmails_RedirectToInternalBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1/", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	withMockDNS(t, func(host string) ([]net.IP, error) {
		if strings.HasPrefix(host, "127.") {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})
	// httptest binds to loopback; bypass initial validation via direct call to client
	// by pointing at a fake public-looking URL would need DNS rewrite. Instead, hit
	// loopback directly with validation disabled to test the CheckRedirect path.
	// Simpler: validateExternalURL already blocks; verify redirect rejection path
	// by exercising the CheckRedirect closure directly.
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	err := httpClient.CheckRedirect(req, nil)
	if err == nil {
		t.Fatalf("expected redirect to loopback to be rejected")
	}
}
