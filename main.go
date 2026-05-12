package main

import (
	"html/template"
	"log"
	"net/http"
	"os"
)

func main() {
	tmpl := template.Must(template.New("").Funcs(template.FuncMap{"urlExt": urlExt}).ParseGlob("templates/*.html"))

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}

	log.Printf("Starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, routes(tmpl)))
}
