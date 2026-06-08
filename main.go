package main

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"
)

func main() {
	gcPercent := 20
	if s := os.Getenv("GC_PERCENT"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 && v <= 100 {
			gcPercent = v
		}
	}
	debug.SetGCPercent(gcPercent)

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
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Print("Shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("forced shutdown: %v", err)
	}
}
