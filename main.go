package main

import (
	"html/template"
	"log"
	"net/http"
	"os"
	"runtime/debug"
)

func main() {
	debug.SetGCPercent(20) // GC aggressively on 512MB Render free tier

	tmpl := template.Must(template.New("").Funcs(template.FuncMap{"urlExt": urlExt}).ParseGlob("templates/*.html"))

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}

	log.Printf("Starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, routes(tmpl)))
}
