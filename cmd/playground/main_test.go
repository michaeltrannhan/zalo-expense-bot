package main

import (
	"io/fs"
	"testing"
)

func TestRequireLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8090", "[::1]:8090", "localhost:8090"} {
		if err := requireLoopback(addr); err != nil {
			t.Errorf("requireLoopback(%q): %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8090", "192.168.1.5:8090", "bad"} {
		if err := requireLoopback(addr); err == nil {
			t.Errorf("requireLoopback(%q) = nil, want error", addr)
		}
	}
}

func TestValidSessionID(t *testing.T) {
	if !validSessionID("local-12345678") {
		t.Fatal("valid session rejected")
	}
	for _, value := range []string{"short", "has spaces 123", "../../escape", "emoji-🧾-123456"} {
		if validSessionID(value) {
			t.Errorf("invalid session %q accepted", value)
		}
	}
}

func TestPlaygroundAssetsEmbedded(t *testing.T) {
	assets, err := fs.Sub(staticFiles, "static")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.html", "styles.css", "app.js"} {
		if _, err := fs.Stat(assets, name); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}
