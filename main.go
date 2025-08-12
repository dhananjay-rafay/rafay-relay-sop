package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

var (
	sopService *SOPService
	logger     *logrus.Logger
)

func main() {
	// Initialize logger
	logger = logrus.New()
	logger.SetLevel(logrus.InfoLevel)
	logger.SetFormatter(&logrus.JSONFormatter{})

	// Initialize SOP service
	var err error
	sopService, err = NewSOPService()
	if err != nil {
		logger.Fatalf("Failed to initialize SOP service: %v", err)
	}

	// Setup router
	router := mux.NewRouter()
	router.HandleFunc("/run", handleRun).Methods("GET")
	router.HandleFunc("/status", handleStatus).Methods("GET")
	router.HandleFunc("/health", handleHealth).Methods("GET")

	// Setup server
	server := &http.Server{
		Addr:         ":8080",
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		logger.Info("Starting server on :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Fatalf("Server forced to shutdown: %v", err)
	}

	logger.Info("Server exited")
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	logger.Info("Received /run request")

	response := map[string]interface{}{
		"status":  "started",
		"message": "SOP execution started",
		"time":    time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)

	// Start SOP execution in background
	go func() {
		if err := sopService.ExecuteSOP(); err != nil {
			logger.Errorf("SOP execution failed: %v", err)
		} else {
			logger.Info("SOP execution completed successfully")
		}
	}()
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	logger.Info("Received /status request")

	status := sopService.GetStatus()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(status)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}
