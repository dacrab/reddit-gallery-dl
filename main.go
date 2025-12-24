package main

import (
	"html/template"
	"log"
	"net/http"
	"os"
)

func main() {
	tmpl, err := template.ParseGlob("templates/*.html")
	if err != nil {
		log.Fatalf("Failed to parse templates: %v", err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}

	log.Printf("Starting Reddit Gallery DL on port %s...", port)
	if err := http.ListenAndServe(":"+port, NewServer(tmpl).Routes()); err != nil {
		log.Fatal(err)
	}
}
