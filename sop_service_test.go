package main

import (
	"testing"
	"time"
)

func TestSOPStatus(t *testing.T) {
	status := SOPStatus{
		Status:    "Running",
		StartTime: time.Now(),
		Logs:      []string{"Test log entry"},
	}

	if status.Status != "Running" {
		t.Errorf("Expected status 'Running', got '%s'", status.Status)
	}

	if len(status.Logs) != 1 {
		t.Errorf("Expected 1 log entry, got %d", len(status.Logs))
	}
}

func TestConfig(t *testing.T) {
	config := &Config{
		AWSRegion:  "us-west-2",
		EKSCluster: "test-cluster",
		Namespace:  "rafay-core",
		NodeGroup:  "nodegroup-relay-services",
		Deployment: "rafay-relay",
	}

	if config.AWSRegion != "us-west-2" {
		t.Errorf("Expected AWS region 'us-west-2', got '%s'", config.AWSRegion)
	}

	if config.EKSCluster != "test-cluster" {
		t.Errorf("Expected EKS cluster 'test-cluster', got '%s'", config.EKSCluster)
	}
}

func TestPodInfo(t *testing.T) {
	pod := PodInfo{
		Namespace: "rafay-core",
		Name:      "rafay-relay-123",
		Node:      "ip-10-0-1-100.ec2.internal",
	}

	if pod.Namespace != "rafay-core" {
		t.Errorf("Expected namespace 'rafay-core', got '%s'", pod.Namespace)
	}

	if pod.Name != "rafay-relay-123" {
		t.Errorf("Expected pod name 'rafay-relay-123', got '%s'", pod.Name)
	}
}

func TestGetEnvOrDefault(t *testing.T) {
	// Test with default value when env var is not set
	result := getEnvOrDefault("NONEXISTENT_VAR", "default-value")
	if result != "default-value" {
		t.Errorf("Expected 'default-value', got '%s'", result)
	}
}
