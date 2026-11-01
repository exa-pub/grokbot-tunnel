package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/sandapi"
)

// Both Go transports offer no ALPN, like Node's https.Agent and undici's
// fetch, even when the base transport was set up for HTTP/2.
func TestGoTransportsOfferNoALPN(t *testing.T) {
	rec := &recorder{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: rec.record}
	server.StartTLS()
	defer server.Close()
	for _, profile := range []sandapi.Profile{sandapi.ComputerRPC, sandapi.AuthREST} {
		if err := goRequest(context.Background(), server, profile); err != nil {
			t.Fatal(profile, err)
		}
		h, err := rec.take()
		if err != nil || len(h.ALPN) != 0 {
			t.Fatal(profile, h.ALPN, err)
		}
	}
}

func TestEmptyHelloFieldsEncodeAsLists(t *testing.T) {
	rec := &recorder{}
	if _, err := rec.record(&tls.ClientHelloInfo{}); err != nil {
		t.Fatal(err)
	}
	h, err := rec.take()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"cipherSuites":[],"curves":[],"versions":[],"alpn":[],"signatures":[]}`
	if string(b) != want {
		t.Fatalf("got %s, want %s", b, want)
	}
}
