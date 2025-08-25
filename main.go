package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	router.HandleFunc("/clear", handleClear).Methods("GET")
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
	sig := <-quit
	logger.Infof("Received signal: %v", sig)

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

	// Check if there's a completed execution first
	if sopService.stateManager != nil {
		if existingState, err := sopService.stateManager.LoadExistingState(); err == nil && existingState != nil {
			if existingState.Status == "Completed" {
				response := map[string]interface{}{
					"status":  "skipped",
					"message": "Previous execution completed successfully. Use /clear to start fresh.",
					"time":    time.Now().UTC(),
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(response)
				return
			}
		}
	}

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
			// Check if this is a "completed execution" error (not a real failure)
			if strings.Contains(err.Error(), "previous execution completed successfully") {
				logger.Infof("SOP execution skipped: %v", err)
			} else {
				logger.Errorf("SOP execution failed: %v", err)
			}
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

func handleClear(w http.ResponseWriter, r *http.Request) {
	logger.Info("Received /clear request")

	if sopService.stateManager != nil {
		if err := sopService.stateManager.ClearOldState(); err != nil {
			logger.Errorf("Failed to clear state: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "Failed to clear state"})
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "State cleared successfully"})
}
