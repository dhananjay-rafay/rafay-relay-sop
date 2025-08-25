package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Shopify/sarama"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// SOPExecutionState represents the state of a SOP execution
type SOPExecutionState struct {
	ExecutionID    string                 `json:"execution_id"`
	Status         string                 `json:"status"` // "Running", "Completed", "Failed", "Resuming"
	CurrentStep    int                    `json:"current_step"`
	TotalSteps     int                    `json:"total_steps"`
	StepResults    map[int]StepResult     `json:"step_results"`
	StartTime      time.Time              `json:"start_time"`
	LastUpdateTime time.Time              `json:"last_update_time"`
	Error          string                 `json:"error,omitempty"`
	Context        map[string]interface{} `json:"context,omitempty"` // Store intermediate data
}

// StepResult represents the result of a single step
type StepResult struct {
	StepNumber int       `json:"step_number"`
	StepName   string    `json:"step_name"`
	Status     string    `json:"status"` // "Pending", "Running", "Completed", "Failed"
	StartTime  time.Time `json:"start_time"`
	EndTime    time.Time `json:"end_time,omitempty"`
	Error      string    `json:"error,omitempty"`
	RetryCount int       `json:"retry_count"`
	MaxRetries int       `json:"max_retries"`
}

// ResilientStateManager handles state persistence and recovery
type ResilientStateManager struct {
	producer    sarama.SyncProducer
	consumer    sarama.Consumer
	topic       string
	executionID string
	state       *SOPExecutionState
	logger      *logrus.Logger
}

// NewResilientStateManager creates a new state manager
func NewResilientStateManager(kafkaBrokers []string, topic string) (*ResilientStateManager, error) {
	config := sarama.NewConfig()
	config.Producer.RequiredAcks = sarama.WaitForAll
	config.Producer.Retry.Max = 5
	config.Producer.Return.Successes = true
	config.Version = sarama.V2_8_0_0

	// Configure for managed Kafka (MSK)
	config.Net.TLS.Enable = false  // Set to true if your managed Kafka requires TLS
	config.Net.SASL.Enable = false // Set to true if your managed Kafka requires SASL

	producer, err := sarama.NewSyncProducer(kafkaBrokers, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kafka producer: %v", err)
	}

	consumer, err := sarama.NewConsumer(kafkaBrokers, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kafka consumer: %v", err)
	}

	// Create topic if it doesn't exist
	if err := createTopicIfNotExists(kafkaBrokers, topic); err != nil {
		// Log warning but don't fail - topic will be auto-created by Kafka
		logrus.Warnf("Failed to create topic %s: %v (will be auto-created)", topic, err)
	}

	return &ResilientStateManager{
		producer:    producer,
		consumer:    consumer,
		topic:       topic,
		executionID: uuid.New().String(),
		logger:      logrus.New(),
	}, nil
}

// createTopicIfNotExists creates a Kafka topic if it doesn't exist
func createTopicIfNotExists(brokers []string, topic string) error {
	config := sarama.NewConfig()
	config.Version = sarama.V2_8_0_0
	config.Net.TLS.Enable = false
	config.Net.SASL.Enable = false

	admin, err := sarama.NewClusterAdmin(brokers, config)
	if err != nil {
		return fmt.Errorf("failed to create cluster admin: %v", err)
	}
	defer admin.Close()

	// Check if topic exists
	topics, err := admin.ListTopics()
	if err != nil {
		return fmt.Errorf("failed to list topics: %v", err)
	}

	if _, exists := topics[topic]; exists {
		return nil // Topic already exists
	}

	// Create topic
	topicDetail := &sarama.TopicDetail{
		NumPartitions:     1,
		ReplicationFactor: 1,
	}

	err = admin.CreateTopic(topic, topicDetail, false)
	if err != nil {
		return fmt.Errorf("failed to create topic %s: %v", topic, err)
	}

	logrus.Infof("Created Kafka topic: %s", topic)
	return nil
}

// InitializeState initializes a new SOP execution state
func (rsm *ResilientStateManager) InitializeState(totalSteps int) error {
	// Generate a new execution ID for fresh execution
	rsm.executionID = uuid.New().String()

	rsm.state = &SOPExecutionState{
		ExecutionID:    rsm.executionID,
		Status:         "Running",
		CurrentStep:    0,
		TotalSteps:     totalSteps,
		StepResults:    make(map[int]StepResult),
		StartTime:      time.Now(),
		LastUpdateTime: time.Now(),
		Context:        make(map[string]interface{}),
	}

	// Initialize all step results
	for i := 0; i < totalSteps; i++ {
		rsm.state.StepResults[i] = StepResult{
			StepNumber: i,
			Status:     "Pending",
			MaxRetries: 3,
		}
	}

	rsm.logger.Infof("Initialized new execution state: %s", rsm.executionID)
	return rsm.persistState()
}

// LoadExistingState attempts to load existing state for recovery
func (rsm *ResilientStateManager) LoadExistingState() (*SOPExecutionState, error) {
	partitionConsumer, err := rsm.consumer.ConsumePartition(rsm.topic, 0, sarama.OffsetOldest)
	if err != nil {
		return nil, fmt.Errorf("failed to create partition consumer: %v", err)
	}
	defer partitionConsumer.Close()

	var latestState *SOPExecutionState
	timeout := time.After(10 * time.Second)

	// Read all messages to find the latest execution state
	for {
		select {
		case msg := <-partitionConsumer.Messages():
			var state SOPExecutionState
			if err := json.Unmarshal(msg.Value, &state); err != nil {
				rsm.logger.Warnf("Failed to unmarshal state message: %v", err)
				continue
			}

			// Keep track of the latest state
			if latestState == nil || state.LastUpdateTime.After(latestState.LastUpdateTime) {
				latestState = &state
			}

		case <-timeout:
			if latestState != nil {
				rsm.logger.Infof("Found existing execution state: %s (status: %s, step: %d)",
					latestState.ExecutionID, latestState.Status, latestState.CurrentStep)
				rsm.state = latestState
				rsm.executionID = latestState.ExecutionID
				return latestState, nil
			}
			return nil, nil // No existing state found
		}
	}
}

// StartStep marks a step as started
func (rsm *ResilientStateManager) StartStep(stepNumber int, stepName string) error {
	if rsm.state == nil {
		return fmt.Errorf("state not initialized")
	}

	stepResult := rsm.state.StepResults[stepNumber]
	stepResult.Status = "Running"
	stepResult.StepName = stepName
	stepResult.StartTime = time.Now()
	stepResult.RetryCount++

	rsm.state.StepResults[stepNumber] = stepResult
	rsm.state.CurrentStep = stepNumber
	rsm.state.LastUpdateTime = time.Now()

	return rsm.persistState()
}

// CompleteStep marks a step as completed
func (rsm *ResilientStateManager) CompleteStep(stepNumber int) error {
	if rsm.state == nil {
		return fmt.Errorf("state not initialized")
	}

	stepResult := rsm.state.StepResults[stepNumber]
	stepResult.Status = "Completed"
	stepResult.EndTime = time.Now()

	rsm.state.StepResults[stepNumber] = stepResult
	rsm.state.LastUpdateTime = time.Now()

	return rsm.persistState()
}

// FailStep marks a step as failed
func (rsm *ResilientStateManager) FailStep(stepNumber int, error string) error {
	if rsm.state == nil {
		return fmt.Errorf("state not initialized")
	}

	stepResult := rsm.state.StepResults[stepNumber]
	stepResult.Status = "Failed"
	stepResult.EndTime = time.Now()
	stepResult.Error = error

	rsm.state.StepResults[stepNumber] = stepResult
	rsm.state.Status = "Failed"
	rsm.state.Error = error
	rsm.state.LastUpdateTime = time.Now()

	return rsm.persistState()
}

// CompleteExecution marks the entire execution as completed
func (rsm *ResilientStateManager) CompleteExecution() error {
	if rsm.state == nil {
		return fmt.Errorf("state not initialized")
	}

	rsm.state.Status = "Completed"
	rsm.state.LastUpdateTime = time.Now()

	return rsm.persistState()
}

// GetCurrentState returns the current state
func (rsm *ResilientStateManager) GetCurrentState() *SOPExecutionState {
	return rsm.state
}

// CanRetryStep checks if a step can be retried
func (rsm *ResilientStateManager) CanRetryStep(stepNumber int) bool {
	if rsm.state == nil {
		return false
	}

	stepResult := rsm.state.StepResults[stepNumber]
	return stepResult.Status == "Failed" && stepResult.RetryCount < stepResult.MaxRetries
}

// SetContext stores intermediate data
func (rsm *ResilientStateManager) SetContext(key string, value interface{}) error {
	if rsm.state == nil {
		return fmt.Errorf("state not initialized")
	}

	rsm.state.Context[key] = value
	rsm.state.LastUpdateTime = time.Now()

	return rsm.persistState()
}

// GetContext retrieves intermediate data
func (rsm *ResilientStateManager) GetContext(key string) interface{} {
	if rsm.state == nil {
		return nil
	}

	return rsm.state.Context[key]
}

// ClearOldState clears the current state and starts fresh
func (rsm *ResilientStateManager) ClearOldState() error {
	rsm.logger.Info("Clearing old execution state")
	rsm.state = nil
	rsm.executionID = uuid.New().String()
	return nil
}

// persistState saves the current state to Kafka
func (rsm *ResilientStateManager) persistState() error {
	if rsm.state == nil {
		return fmt.Errorf("no state to persist")
	}

	rsm.state.LastUpdateTime = time.Now()
	data, err := json.Marshal(rsm.state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %v", err)
	}

	msg := &sarama.ProducerMessage{
		Topic: rsm.topic,
		Key:   sarama.StringEncoder(rsm.executionID),
		Value: sarama.ByteEncoder(data),
	}

	partition, offset, err := rsm.producer.SendMessage(msg)
	if err != nil {
		// Check if it's a topic not found error
		if saramaErr, ok := err.(sarama.ProducerError); ok {
			if saramaErr.Err == sarama.ErrUnknownTopicOrPartition {
				rsm.logger.Warnf("Kafka topic %s does not exist, state persistence disabled", rsm.topic)
				return fmt.Errorf("topic not found: %s", rsm.topic)
			}
		}
		return fmt.Errorf("failed to send message to Kafka: %v", err)
	}

	rsm.logger.WithFields(logrus.Fields{
		"execution_id": rsm.executionID,
		"partition":    partition,
		"offset":       offset,
		"step":         rsm.state.CurrentStep,
		"status":       rsm.state.Status,
	}).Info("State persisted to Kafka")

	return nil
}

// Close closes the Kafka connections
func (rsm *ResilientStateManager) Close() error {
	rsm.logger.Info("Closing Kafka connections...")
	if err := rsm.producer.Close(); err != nil {
		rsm.logger.Warnf("Failed to close producer: %v", err)
	}
	if err := rsm.consumer.Close(); err != nil {
		rsm.logger.Warnf("Failed to close consumer: %v", err)
	}
	rsm.logger.Info("Kafka connections closed")
	return nil
}
