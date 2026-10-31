// native-app is the e2e workload for native deploys: it serves /up, and /env
// echoes MSG so the test can check that systemd delivered the env file's
// value byte for byte.
package main

import (
	"log"
	"net/http"
	"os"
)

var version = "dev"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/up", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(version)) })
	mux.HandleFunc("/env", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(os.Getenv("MSG"))) })
	mux.HandleFunc("/write", func(w http.ResponseWriter, _ *http.Request) {
		if err := os.WriteFile("storage/probe", []byte(version), 0o644); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write([]byte("wrote"))
	})
	log.Printf("native-app %s listening on :8080", version)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
