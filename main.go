package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/store"
)

func main() {

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	// create a logger
	var standard_logger = log.New(os.Stderr, "DEBUG: ", log.LstdFlags)
	file, err := os.OpenFile("linko.access.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		standard_logger.Printf("failed to open access log: %v\n", err)
		return 1
	}

	var access_logger = log.New(file, "INFO: ", log.LstdFlags)

	st, err := store.New(dataDir, standard_logger)
	if err != nil {
		standard_logger.Printf("failed to create store: %v", err)
		return 1
	}
	s := newServer(*st, httpPort, cancel, access_logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	standard_logger.Println("Linko is shutting down")
	if err := s.shutdown(shutdownCtx); err != nil {
		standard_logger.Printf("failed to shutdown server: %v", err)
		return 1
	}
	if serverErr != nil {
		standard_logger.Printf("server error: %v", serverErr)
		return 1
	}
	return 0
}
