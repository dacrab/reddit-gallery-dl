package main

import (
	"html/template"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	tmpl, err := template.ParseGlob("templates/*.html")
	if err != nil {
		slog.Error("Failed to parse templates", "error", err)
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}

	slog.Info("Starting Reddit Gallery DL", "port", port)
	if err := http.ListenAndServe(":"+port, NewServer(tmpl).Routes()); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}
