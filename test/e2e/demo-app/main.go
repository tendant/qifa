package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

func main() {
	version := envOr("APP_VERSION", "dev")
	mux := http.NewServeMux()
	mux.HandleFunc("/up", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"version": version,
		})
	})

	log.Printf("demo-app starting version=%s", version)
	if err := http.ListenAndServe(":3000", mux); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
