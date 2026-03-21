package main

import (
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"hasSuffix": strings.HasSuffix,
	}).ParseGlob("templates/*.html")
	if err != nil {
		slog.Error("Failed to parse templates", "error", err)
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           NewServer(tmpl).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      150 * time.Second, // must exceed Reddit client timeout
		IdleTimeout:       120 * time.Second,
	}

	slog.Info("Starting Reddit Gallery DL", "port", port)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}
