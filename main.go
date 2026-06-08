package main

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
)

func main() {
	debug.SetGCPercent(20)

	tmpl := template.Must(template.New("").Funcs(template.FuncMap{"urlExt": urlExt}).ParseGlob("templates/*.html"))

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      routes(tmpl),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("Starting on :%s", port)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Print("Shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
}
