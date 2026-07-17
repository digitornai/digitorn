package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	token := os.Getenv("WEBPROBE_TOKEN")
	addr := os.Getenv("WEBPROBE_ADDR")
	if token == "" || addr == "" {
		fmt.Fprintln(os.Stderr, "webprobe: WEBPROBE_TOKEN and WEBPROBE_ADDR are required")
		os.Exit(2)
	}
	page := "<!doctype html><html><head><title>Probe Page</title></head>" +
		"<body><main><h1>Digitorn web probe</h1><p>" + token + "</p></main></body></html>"
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "webprobe:", err)
		os.Exit(1)
	}
}
