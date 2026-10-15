package tracepanel

import (
	"net/http"
	"testing"
)

// Code that asserts the default transport is an *http.Transport (to clone it) keeps working.
func TestTheDefaultTransportStaysAnHTTPTransport(t *testing.T) {
	before := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = before })
	http.DefaultClient.Transport = nil

	installTransport()

	if _, ok := http.DefaultTransport.(*http.Transport); !ok {
		t.Fatalf("http.DefaultTransport is a %T", http.DefaultTransport)
	}
	if _, ok := http.DefaultClient.Transport.(*transport); !ok {
		t.Fatalf("http.DefaultClient's transport is a %T", http.DefaultClient.Transport)
	}
}
