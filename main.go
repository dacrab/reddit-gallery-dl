package main

import (
	"html/template"
	"log"
	"net/http"
	"os"
)

func main() {
	// 1. Initialize Templates
	tmpl, err := template.ParseGlob("templates/*.html")
	if err != nil {
		log.Fatalf("Critical: failed to parse templates: %v", err)
	}

	// 2. Setup Server
	server := NewServer(tmpl)

	// 3. Configure Port
	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}

	// 4. Start
	log.Printf("Starting Reddit Gallery DL on port %s...", port)
	if err := http.ListenAndServe(":"+port, server.Routes()); err != nil {
		log.Fatal(err)
	}
}
