package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type SOPStatus struct {
	Status    string     `json:"status"` // Running, Success, Failed
	StartTime time.Time  `json:"start_time"`
	EndTime   *time.Time `json:"end_time,omitempty"`
	Logs      []string   `json:"logs"`
	Error     string     `json:"error,omitempty"`
}

type SOPService struct {
	mu           sync.RWMutex
	status       SOPStatus
	runStateMu   sync.Mutex
	runInFlight  bool
	k8sClient    *kubernetes.Clientset
	awsConfig    aws.Config
	eksClient    *eks.Client
	asgClient    *autoscaling.Client
	ec2Client    *ec2.Client
	config       *Config
	awsOps       *AWSOperations
	k8sOps       *K8SOperations
	stateManager *ResilientStateManager
	oldPods      []PodInfo
	oldNodes     []string
}

type Config struct {
	AWSRegion        string
	EKSCluster       string
	Namespace        string
	NodeGroup        string
	Deployment       string
	SentryDeployment string
}

func NewSOPService() (*SOPService, error) {
	// Load AWS config
	awsCfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %v", err)
	}

	// Load config from configmap
	cfg, err := loadConfigFromConfigMap()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %v", err)
	}

	// Initialize Kubernetes client
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %v", err)
	}

	k8sClient, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %v", err)
	}

	// Ensure the original AWS config has the correct region
	awsCfg.Region = cfg.AWSRegion

	// Log the configuration for debugging
	logrus.Infof("AWS Region from config: %s", cfg.AWSRegion)
	logrus.Infof("AWS Config Region: %s", awsCfg.Region)

	// Initialize AWS clients with the modified config
	eksClient := eks.NewFromConfig(awsCfg)
	asgClient := autoscaling.NewFromConfig(awsCfg)
	ec2Client := ec2.NewFromConfig(awsCfg)

	// Initialize operations modules
	awsOps := NewAWSOperations(eksClient, asgClient, ec2Client, cfg.AWSRegion)
	k8sOps := NewK8SOperations(k8sClient, cfg.Namespace)

	// Initialize resilient state manager
	kafkaBrokers := []string{getEnvOrDefault("KAFKA_BROKERS", "localhost:9092")}
	kafkaTopic := getEnvOrDefault("KAFKA_TOPIC", "sop-execution-state")

	stateManager, err := NewResilientStateManager(kafkaBrokers, kafkaTopic)
	if err != nil {
		logrus.Warnf("Failed to initialize state manager: %v, continuing without resilience", err)
		stateManager = nil
	}

	return &SOPService{
		status: SOPStatus{
			Status:    "Idle",
			StartTime: time.Time{},
			Logs:      []string{},
		},
		k8sClient:    k8sClient,
		awsConfig:    awsCfg,
		eksClient:    eksClient,
		asgClient:    asgClient,
		ec2Client:    ec2Client,
		config:       cfg,
		awsOps:       awsOps,
		k8sOps:       k8sOps,
		stateManager: stateManager,
	}, nil
}

func loadConfigFromConfigMap() (*Config, error) {
	// This would typically load from a configmap
	// For now, we'll use environment variables or defaults
	return &Config{
		AWSRegion:        getEnvOrDefault("AWS_REGION", "us-east-1"),
		EKSCluster:       getEnvOrDefault("EKS_CLUSTER", "upgrade-tb"),
		Namespace:        "rafay-core",
		NodeGroup:        "nodegroup-relay-services",
		Deployment:       "rafay-relay",
		SentryDeployment: getEnvOrDefault("SENTRY_DEPLOYMENT", "rafay-sentry"),
	}, nil
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := getEnv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnv(key string) string {
	// Read environment variables from ConfigMap
	return os.Getenv(key)
}

func (s *SOPService) tryAcquireExecution() bool {
	s.runStateMu.Lock()
	defer s.runStateMu.Unlock()
	if s.runInFlight {
		return false
	}
	s.runInFlight = true
	return true
}

func (s *SOPService) IsExecutionRunning() bool {
	s.runStateMu.Lock()
	defer s.runStateMu.Unlock()
	return s.runInFlight
}

func (s *SOPService) releaseExecution() {
	s.runStateMu.Lock()
	s.runInFlight = false
	s.runStateMu.Unlock()
}

func isSkipExecutionErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "previous execution completed successfully")
}

func (s *SOPService) ExecuteSOP() (err error) {
	// Check for existing state and attempt recovery
	if s.stateManager != nil {
		if existingState, err := s.stateManager.LoadExistingState(); err == nil && existingState != nil {
			s.addLog(fmt.Sprintf("Found existing execution state: %s (status: %s, step: %d)",
				existingState.ExecutionID, existingState.Status, existingState.CurrentStep))

			// Detect incompatible step layout (e.g. state created before sentry step was added).
			// Old executions used 12 steps; current code uses 13.
			const currentTotalSteps = 13
			if existingState.TotalSteps != currentTotalSteps {
				s.addLog(fmt.Sprintf("Detected execution state with %d steps, but current SOP expects %d steps; reverting cluster changes then clearing old state to avoid mismatched resumption", existingState.TotalSteps, currentTotalSteps))

				// Best-effort revert of any changes made by the old execution (scale/cordon, etc.)
				s.RevertSteps(existingState)

				if err := s.stateManager.ClearOldState(); err != nil {
					s.addLog(fmt.Sprintf("Warning: Failed to clear incompatible old state: %v", err))
				}
				// Treat as if no existing state was found
				existingState = nil
			}

			// If after compatibility check we still have a state, handle it as before
			if existingState != nil {

				if existingState.Status == "Running" || existingState.Status == "Resuming" {
					s.addLog("Attempting to resume execution from previous state")
					return s.resumeExecution(existingState)
				} else if existingState.Status == "Completed" {
					s.addLog("Previous execution was completed successfully")
					s.addLog("To start a new execution, clear the state first using /clear endpoint")
					// Restore the completed state for status reporting
					s.mu.Lock()
					s.status = SOPStatus{
						Status:    "Success",
						StartTime: existingState.StartTime,
						EndTime:   &existingState.LastUpdateTime,
						Logs:      []string{fmt.Sprintf("Execution completed successfully at %s", existingState.LastUpdateTime.Format(time.RFC3339))},
					}
					s.mu.Unlock()
					return fmt.Errorf("previous execution completed successfully - use /clear to start fresh") // Return error to indicate no new execution
				} else if existingState.Status == "Cleared" {
					s.addLog("Previous execution state was cleared, starting fresh execution")
					// State was cleared, start fresh
					if err := s.stateManager.ClearOldState(); err != nil {
						s.addLog(fmt.Sprintf("Warning: Failed to clear old state: %v", err))
					}
				} else if existingState.Status == "Failed" {
					s.addLog("Previous execution failed, starting fresh execution")
					// Clear the failed state and start fresh
					if err := s.stateManager.ClearOldState(); err != nil {
						s.addLog(fmt.Sprintf("Warning: Failed to clear old state: %v", err))
					}
				}
			}
		}
	}

	// Initialize new execution
	s.mu.Lock()
	s.status = SOPStatus{
		Status:    "Running",
		StartTime: time.Now(),
		Logs:      []string{},
	}
	s.mu.Unlock()

	// Initialize resilient state if available
	if s.stateManager != nil {
		if err := s.stateManager.InitializeState(13); err != nil {
			s.addLog(fmt.Sprintf("Warning: Failed to initialize resilient state: %v", err))
			// Disable state manager if initialization fails
			s.stateManager = nil
		}
	}

	defer func() {
		s.mu.Lock()
		endTime := time.Now()
		s.status.EndTime = &endTime
		if err != nil && !isSkipExecutionErr(err) {
			s.status.Status = "Failed"
			s.status.Error = err.Error()
		} else if s.status.Status == "Running" {
			s.status.Status = "Success"
		}
		s.mu.Unlock()

		// Mark execution as completed in resilient state only on success (or skip)
		if s.stateManager != nil && (err == nil || isSkipExecutionErr(err)) {
			_ = s.stateManager.CompleteExecution()
		}
	}()

	steps := []struct {
		name string
		fn   func() error
	}{
		{"Get AWS region and EKS cluster from configmap", s.stepGetConfig},
		{"Find nodegroup with label Name=nodegroup-relay-services and nodecount >1", s.stepFindNodeGroup},
		{"Get list of nodes with old relay pods", s.stepGetOldRelayNodes},
		{"Cordon nodes with old relay pods", s.stepCordonNodes},
		{"Scale nodegroup to 2X current count", s.stepScaleNodeGroup},
		{"Wait for new nodes to join cluster", s.stepWaitForNewNodes},
		{"Rollout restart rafay-sentry deployment", s.stepRestartSentryDeployment},
		{"Scale rafay-relay deployment to 2x", s.stepScaleRelayDeployment},
		{"Wait for new pods to be running", s.stepWaitForNewPods},
		{"Delete old pods and scale back deployment", s.stepDeleteOldPods},
		{"Remove scale in protections", s.stepRemoveScaleInProtections},
		{"Add scale in protection for new nodes", s.stepAddScaleInProtections},
		{"Scale nodegroup back to original size", s.stepScaleNodeGroupBack},
	}

	for i, step := range steps {
		// Check if step should be skipped (already completed)
		if s.stateManager != nil {
			state := s.stateManager.GetCurrentState()
			if state != nil && state.StepResults[i].Status == "Completed" {
				s.addLog(fmt.Sprintf("Skipping completed step: %s", step.name))
				continue
			}
		}

		// Start step tracking
		if s.stateManager != nil {
			if err := s.stateManager.StartStep(i, step.name); err != nil {
				// Only log once per step to reduce noise
				if i == 0 {
					s.addLog(fmt.Sprintf("Warning: State persistence disabled due to Kafka issues: %v", err))
				}
			}
		}

		s.addLog(fmt.Sprintf("Starting step: %s", step.name))

		// Execute step with retry logic
		if err := s.executeStepWithRetry(step.fn, i); err != nil {
			s.addLog(fmt.Sprintf("Step failed: %s - %v", step.name, err))

			// Mark step as failed in resilient state
			if s.stateManager != nil {
				if err := s.stateManager.FailStep(i, err.Error()); err != nil {
					// Don't log state persistence failures for step failures
				}

				// On execution failure, attempt rollback based on current state
				if state := s.stateManager.GetCurrentState(); state != nil {
					s.addLog("Execution failed, initiating automatic rollback of completed steps")
					s.RevertSteps(state)
				}
			}

			s.mu.Lock()
			s.status.Status = "Failed"
			s.status.Error = err.Error()
			s.mu.Unlock()
			return err
		}

		// Mark step as completed
		if s.stateManager != nil {
			if err := s.stateManager.CompleteStep(i); err != nil {
				// Don't log every step completion failure to reduce noise
				// The first failure was already logged
			}
		}

		s.addLog(fmt.Sprintf("Completed step: %s", step.name))
	}

	return nil
}

func (s *SOPService) stepGetConfig() error {
	s.addLog(fmt.Sprintf("AWS Region: %s, EKS Cluster: %s", s.config.AWSRegion, s.config.EKSCluster))
	return nil
}

func (s *SOPService) stepFindNodeGroup() error {
	s.addLog("Finding nodegroup with label Name=nodegroup-relay-services")

	nodeGroupName, err := s.awsOps.FindNodeGroup(s.config.EKSCluster, s.config.NodeGroup)
	if err != nil {
		return fmt.Errorf("failed to find nodegroup: %v", err)
	}

	s.addLog(fmt.Sprintf("Found nodegroup: %s", *nodeGroupName))

	// Get current nodegroup details
	asgName, err := s.awsOps.GetNodeGroupASG(s.config.EKSCluster, *nodeGroupName)
	if err != nil {
		s.addLog(fmt.Sprintf("Warning: Could not get ASG for nodegroup: %v", err))
	} else {
		s.addLog(fmt.Sprintf("Nodegroup ASG: %s", *asgName))

		// Get current instance count
		instances, err := s.awsOps.GetASGInstances(*asgName)
		if err != nil {
			s.addLog(fmt.Sprintf("Warning: Could not get ASG instances: %v", err))
		} else {
			s.addLog(fmt.Sprintf("Current ASG instances (%d): %v", len(instances), instances))
		}
	}

	return nil
}

func (s *SOPService) stepScaleNodeGroup() error {
	s.addLog("Scaling nodegroup to 2X current count")

	// Find the nodegroup first
	nodeGroupName, err := s.awsOps.FindNodeGroup(s.config.EKSCluster, s.config.NodeGroup)
	if err != nil {
		return fmt.Errorf("failed to find nodegroup: %v", err)
	}

	// Get current nodegroup scaling configuration
	currentDesiredSize, err := s.awsOps.GetNodeGroupScalingConfig(s.config.EKSCluster, *nodeGroupName)
	if err != nil {
		return fmt.Errorf("failed to get nodegroup scaling config: %v", err)
	}

	targetCount := currentDesiredSize * 2

	s.addLog(fmt.Sprintf("Current nodegroup desired size: %d", currentDesiredSize))
	s.addLog(fmt.Sprintf("Target nodegroup size: %d", targetCount))

	// Persist original size and nodegroup name for revert on clear
	if s.stateManager != nil {
		_ = s.stateManager.SetContext("originalNodegroupSize", int(currentDesiredSize))
		_ = s.stateManager.SetContext("nodegroupName", *nodeGroupName)
	}

	// Check if already at target size (idempotency check)
	if currentDesiredSize >= targetCount {
		s.addLog(fmt.Sprintf("Nodegroup %s is already at or above target size (%d >= %d), skipping scaling", *nodeGroupName, currentDesiredSize, targetCount))
		return nil
	}

	s.addLog(fmt.Sprintf("Scaling EKS nodegroup %s from %d to %d nodes", *nodeGroupName, currentDesiredSize, targetCount))

	// Scale the EKS nodegroup directly
	err = s.awsOps.ScaleEKSNodeGroup(s.config.EKSCluster, *nodeGroupName, targetCount)
	if err != nil {
		return fmt.Errorf("failed to scale EKS nodegroup: %v", err)
	}

	s.addLog(fmt.Sprintf("Successfully initiated scaling of EKS nodegroup %s to %d nodes", *nodeGroupName, targetCount))
	s.addLog("Note: EKS nodegroup scaling may take several minutes to complete")

	return nil
}

func (s *SOPService) stepCheckASGStatus() error {
	s.addLog("Checking EKS nodegroup scaling status")

	// Find the nodegroup first
	nodeGroupName, err := s.awsOps.FindNodeGroup(s.config.EKSCluster, s.config.NodeGroup)
	if err != nil {
		return fmt.Errorf("failed to find nodegroup: %v", err)
	}

	// Get current nodegroup scaling configuration
	currentDesiredSize, err := s.awsOps.GetNodeGroupScalingConfig(s.config.EKSCluster, *nodeGroupName)
	if err != nil {
		return fmt.Errorf("failed to get nodegroup scaling config: %v", err)
	}

	s.addLog(fmt.Sprintf("EKS nodegroup %s current desired size: %d", *nodeGroupName, currentDesiredSize))

	// Get the ASG name for instance count
	asgName, err := s.awsOps.GetNodeGroupASG(s.config.EKSCluster, *nodeGroupName)
	if err != nil {
		s.addLog(fmt.Sprintf("Warning: Could not get ASG for nodegroup: %v", err))
	} else {
		// Get current instance count
		instances, err := s.awsOps.GetASGInstances(*asgName)
		if err != nil {
			s.addLog(fmt.Sprintf("Warning: Could not get ASG instances: %v", err))
		} else {
			s.addLog(fmt.Sprintf("ASG %s current instances (%d): %v", *asgName, len(instances), instances))

			// Check if scaling is still in progress
			if int32(len(instances)) < currentDesiredSize {
				s.addLog(fmt.Sprintf("Warning: Nodegroup scaling may still be in progress. Expected %d instances, got %d", currentDesiredSize, len(instances)))
			} else {
				s.addLog("Nodegroup scaling appears to be complete")
			}
		}
	}

	return nil
}

func (s *SOPService) stepWaitForNewNodes() error {
	s.addLog("Waiting for new relay nodes to join the cluster and be ready")

	// Get initial relay node count and store their names
	initialRelayNodes, err := s.k8sOps.GetRelayNodes()
	if err != nil {
		return fmt.Errorf("failed to get initial relay node count: %v", err)
	}

	initialCount := len(initialRelayNodes)
	s.addLog(fmt.Sprintf("Initial relay node count: %d", initialCount))

	// Store the names of initial nodes to identify new ones later
	initialNodeNames := make(map[string]bool)
	for _, node := range initialRelayNodes {
		initialNodeNames[node.Name] = true
		s.addLog(fmt.Sprintf("Initial relay node: %s (status: %s)", node.Name, node.Status))
	}

	// Get target count from EKS nodegroup
	nodeGroupName, err := s.awsOps.FindNodeGroup(s.config.EKSCluster, s.config.NodeGroup)
	if err != nil {
		return fmt.Errorf("failed to find nodegroup: %v", err)
	}

	targetDesiredSize, err := s.awsOps.GetNodeGroupScalingConfig(s.config.EKSCluster, *nodeGroupName)
	if err != nil {
		return fmt.Errorf("failed to get nodegroup scaling config: %v", err)
	}

	// We need to wait for (targetDesiredSize - initialCount) new nodes to join.
	// e.g. scale 3->6: initialCount=3, targetDesiredSize=6, so we wait for 3 new nodes.
	targetNewNodes := int(targetDesiredSize) - initialCount
	if targetNewNodes <= 0 {
		s.addLog("No new nodes expected (nodegroup already at or above target); step succeeds")
		return nil
	}
	s.addLog(fmt.Sprintf("Target new relay node count: %d", targetNewNodes))

	// Wait for new relay nodes to join and be ready (with timeout)
	maxWaitTime := 10 * time.Minute
	checkInterval := 30 * time.Second
	startTime := time.Now()

	for time.Since(startTime) < maxWaitTime {
		currentRelayNodes, err := s.k8sOps.GetRelayNodes()
		if err != nil {
			s.addLog(fmt.Sprintf("Warning: Could not get current relay node count: %v", err))
		} else {
			// Identify new nodes (nodes not in initial list)
			var newNodes []NodeInfo
			for _, node := range currentRelayNodes {
				if !initialNodeNames[node.Name] {
					newNodes = append(newNodes, node)
				}
			}

			newNodeCount := len(newNodes)
			s.addLog(fmt.Sprintf("Current new relay node count: %d (target: %d)", newNodeCount, targetNewNodes))

			// Check if all new relay nodes are ready
			readyNewCount := 0
			for _, node := range newNodes {
				if node.Status == "Ready" {
					readyNewCount++
					s.addLog(fmt.Sprintf("New relay node ready: %s", node.Name))
				} else {
					s.addLog(fmt.Sprintf("New relay node not ready: %s (status: %s)", node.Name, node.Status))
				}
			}

			s.addLog(fmt.Sprintf("Ready new relay nodes: %d/%d", readyNewCount, newNodeCount))

			if newNodeCount >= targetNewNodes && readyNewCount >= targetNewNodes {
				s.addLog(fmt.Sprintf("Success! All %d new relay nodes have joined the cluster and are ready", targetNewNodes))
				return nil
			}
		}

		s.addLog(fmt.Sprintf("Waiting %s for more new relay nodes to join and be ready...", checkInterval))
		time.Sleep(checkInterval)
	}

	return fmt.Errorf("timeout waiting for new relay nodes to join cluster and be ready after %v", maxWaitTime)
}

func (s *SOPService) stepGetOldRelayNodes() error {
	s.addLog("Getting list of nodes with old relay pods (excluding SOP pod)")

	pods, err := s.k8sOps.GetRelayPods()
	if err != nil {
		return fmt.Errorf("failed to get relay pods: %v", err)
	}

	// Filter out SOP pods and pods that are not yet scheduled on a node (Node == "" or "<none>")
	var oldPods []PodInfo
	for _, pod := range pods {
		if strings.Contains(pod.Name, "rafay-relay-sop") {
			continue
		}
		if pod.Node == "" || pod.Node == "<none>" {
			// Pod is not scheduled on any node yet; skip it so we don't try to cordon "<none>"
			continue
		}
		oldPods = append(oldPods, pod)
	}

	s.addLog(fmt.Sprintf("Found %d old relay pods (excluding SOP and unscheduled pods)", len(oldPods)))
	for _, pod := range oldPods {
		s.addLog(fmt.Sprintf("  - %s/%s on node %s", pod.Namespace, pod.Name, pod.Node))
	}

	// Store old pods for later use
	s.oldPods = oldPods

	// Get unique nodes
	nodeMap := make(map[string]bool)
	for _, pod := range oldPods {
		nodeMap[pod.Node] = true
	}

	s.addLog(fmt.Sprintf("Old relay pods running on %d unique nodes", len(nodeMap)))
	for nodeName := range nodeMap {
		s.addLog(fmt.Sprintf("  - Node: %s", nodeName))
	}

	// Store old nodes for later use
	s.oldNodes = make([]string, 0, len(nodeMap))
	for nodeName := range nodeMap {
		s.oldNodes = append(s.oldNodes, nodeName)
	}

	// Save context data to resilient state
	if s.stateManager != nil {
		if err := s.stateManager.SetContext("oldPods", s.oldPods); err != nil {
			// Don't log context saving failures to reduce noise
		}
		if err := s.stateManager.SetContext("oldNodes", s.oldNodes); err != nil {
			// Don't log context saving failures to reduce noise
		}
	}

	return nil
}

func (s *SOPService) stepCordonNodes() error {
	s.addLog("Cordoning nodes with old relay pods")

	// Use the stored old nodes from previous step
	if len(s.oldNodes) == 0 {
		return fmt.Errorf("no old nodes found to cordon")
	}

	s.addLog(fmt.Sprintf("Found %d unique nodes to cordon", len(s.oldNodes)))

	// Debug: Show all node names
	s.addLog("Node names to cordon:")
	for _, nodeName := range s.oldNodes {
		s.addLog(fmt.Sprintf("  - %s", nodeName))
	}

	var cordonedNodeNames []string
	// Cordon each node
	for _, nodeName := range s.oldNodes {
		// Extra safety: skip any bogus entries that somehow slipped through
		if nodeName == "" || nodeName == "<none>" {
			s.addLog(fmt.Sprintf("Skipping invalid node name: %q", nodeName))
			continue
		}

		s.addLog(fmt.Sprintf("Processing node: %s", nodeName))

		// Check if nodeName is an IP address and convert to node name if needed
		actualNodeName := nodeName
		if strings.Contains(nodeName, ".") {
			s.addLog(fmt.Sprintf("Converting IP address %s to node name", nodeName))
			convertedName, err := s.k8sOps.GetNodeNameByIP(nodeName)
			if err != nil {
				s.addLog(fmt.Sprintf("Warning: Could not convert IP %s to node name: %v", nodeName, err))
				// Try with the original name anyway
			} else {
				actualNodeName = convertedName
				s.addLog(fmt.Sprintf("Converted IP %s to node name: %s", nodeName, actualNodeName))
			}
		}

		s.addLog(fmt.Sprintf("Cordoning node: %s", actualNodeName))
		err := s.k8sOps.CordonNode(actualNodeName)
		if err != nil {
			return fmt.Errorf("failed to cordon node %s: %v", actualNodeName, err)
		}
		cordonedNodeNames = append(cordonedNodeNames, actualNodeName)
		s.addLog(fmt.Sprintf("Successfully cordoned node: %s", actualNodeName))
	}

	// Persist cordoned node names for revert on clear
	if s.stateManager != nil && len(cordonedNodeNames) > 0 {
		_ = s.stateManager.SetContext("cordonedNodes", cordonedNodeNames)
	}

	s.addLog(fmt.Sprintf("Successfully cordoned %d nodes", len(cordonedNodeNames)))
	return nil
}

func (s *SOPService) stepScaleRelayDeployment() error {
	// First, inspect actual pods to see if there are pending ones
	pods, err := s.k8sOps.GetRelayPods()
	if err != nil {
		return fmt.Errorf("failed to get relay pods before scaling deployment: %v", err)
	}

	var scheduledCount int32
	var pendingCount int32
	for _, pod := range pods {
		if pod.Node == "" || pod.Node == "<none>" {
			pendingCount++
		} else {
			scheduledCount++
		}
	}

	var currentReplicas int32

	if pendingCount > 0 && scheduledCount > 0 {
		// We already have extra pods in Pending; normalize replicas to match scheduled pods
		s.addLog(fmt.Sprintf("Found %d pending relay pods and %d scheduled pods; normalizing deployment %s to %d replicas before scaling",
			pendingCount, scheduledCount, s.config.Deployment, scheduledCount))

		if err := s.k8sOps.ScaleDeployment(s.config.Deployment, scheduledCount); err != nil {
			return fmt.Errorf("failed to normalize deployment replicas before scaling: %v", err)
		}
		currentReplicas = scheduledCount
	} else {
		// No pending pods (or no scheduled ones) – use the spec replicas as baseline
		currentReplicas, err = s.k8sOps.GetDeploymentReplicas(s.config.Deployment)
		if err != nil {
			return fmt.Errorf("failed to get deployment replicas: %v", err)
		}
	}

	newReplicas := currentReplicas * 2

	s.addLog(fmt.Sprintf("Current deployment replicas (baseline for scaling): %d", currentReplicas))
	s.addLog(fmt.Sprintf("Target deployment replicas: %d", newReplicas))

	// Check if already at target replicas (idempotency check)
	if currentReplicas >= newReplicas {
		s.addLog(fmt.Sprintf("Deployment %s is already at or above target replicas (%d >= %d), skipping scaling", s.config.Deployment, currentReplicas, newReplicas))
		return nil
	}

	// Scale the deployment
	if err := s.k8sOps.ScaleDeployment(s.config.Deployment, newReplicas); err != nil {
		return fmt.Errorf("failed to scale deployment: %v", err)
	}

	s.addLog(fmt.Sprintf("Scaled deployment from %d to %d replicas", currentReplicas, newReplicas))
	return nil
}

func (s *SOPService) stepWaitForNewPods() error {
	s.addLog("Waiting for new pods to be running")

	// Wait for deployment to be ready
	err := s.k8sOps.WaitForDeploymentReady(s.config.Deployment, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("failed to wait for deployment to be ready: %v", err)
	}

	s.addLog("Deployment is ready with new pods")

	// Get current pods to verify they're running
	pods, err := s.k8sOps.GetRelayPods()
	if err != nil {
		return fmt.Errorf("failed to get relay pods: %v", err)
	}

	s.addLog(fmt.Sprintf("Found %d relay pods after scaling", len(pods)))
	for _, pod := range pods {
		s.addLog(fmt.Sprintf("  - %s/%s on node %s", pod.Namespace, pod.Name, pod.Node))
	}

	return nil
}

func (s *SOPService) stepRestartSentryDeployment() error {
	s.addLog(fmt.Sprintf("Rolling out restart of sentry deployment %s", s.config.SentryDeployment))

	if err := s.k8sOps.RestartDeployment(s.config.SentryDeployment); err != nil {
		return fmt.Errorf("failed to restart sentry deployment %s: %v", s.config.SentryDeployment, err)
	}

	// Wait for all sentry pods to be available
	if err := s.k8sOps.WaitForDeploymentReady(s.config.SentryDeployment, 5*time.Minute); err != nil {
		return fmt.Errorf("failed to wait for sentry deployment %s to be ready: %v", s.config.SentryDeployment, err)
	}

	s.addLog(fmt.Sprintf("Sentry deployment %s successfully restarted and all pods are available", s.config.SentryDeployment))
	return nil
}

func (s *SOPService) stepScaleNodeGroupBack() error {
	s.addLog("Scaling nodegroup back to original size")

	// Find the nodegroup first
	nodeGroupName, err := s.awsOps.FindNodeGroup(s.config.EKSCluster, s.config.NodeGroup)
	if err != nil {
		return fmt.Errorf("failed to find nodegroup: %v", err)
	}

	// Get current nodegroup scaling configuration
	currentDesiredSize, err := s.awsOps.GetNodeGroupScalingConfig(s.config.EKSCluster, *nodeGroupName)
	if err != nil {
		return fmt.Errorf("failed to get nodegroup scaling config: %v", err)
	}

	originalSize := currentDesiredSize / 2 // Scale back to original size
	s.addLog(fmt.Sprintf("Current nodegroup desired size: %d", currentDesiredSize))
	s.addLog(fmt.Sprintf("Target nodegroup size: %d", originalSize))

	// Check if already at or below target size (idempotency check)
	if currentDesiredSize <= originalSize {
		s.addLog(fmt.Sprintf("Nodegroup %s is already at or below target size (%d <= %d), skipping scaling", *nodeGroupName, currentDesiredSize, originalSize))
		return nil
	}

	s.addLog(fmt.Sprintf("Scaling EKS nodegroup %s back to %d nodes", *nodeGroupName, originalSize))

	// Scale the EKS nodegroup back to original size
	err = s.awsOps.ScaleEKSNodeGroup(s.config.EKSCluster, *nodeGroupName, originalSize)
	if err != nil {
		return fmt.Errorf("failed to scale EKS nodegroup back: %v", err)
	}

	s.addLog(fmt.Sprintf("Successfully initiated scaling of EKS nodegroup %s back to %d nodes", *nodeGroupName, originalSize))
	s.addLog("Note: Nodegroup scaling back may take several minutes to complete")

	return nil
}

// RevertSteps undoes cluster changes from a previous SOP run (e.g. before clear).
// It uncordons any nodes that were cordoned and scales the nodegroup back to original size.
func (s *SOPService) RevertSteps(state *SOPExecutionState) {
	if state == nil || state.Context == nil {
		return
	}
	ctx := state.Context

	// 1. Scale nodegroup back to original size (undo 2x scale-up)
	if ngNameRaw, ok := ctx["nodegroupName"]; ok && ngNameRaw != nil {
		ngName, _ := ngNameRaw.(string)
		origSizeRaw, hasSize := ctx["originalNodegroupSize"]
		if ngName != "" && hasSize && origSizeRaw != nil {
			var originalSize int32
			switch v := origSizeRaw.(type) {
			case float64:
				originalSize = int32(v)
			case int:
				originalSize = int32(v)
			case int32:
				originalSize = v
			default:
				logrus.Warnf("Revert: unexpected originalNodegroupSize type: %T", origSizeRaw)
				originalSize = 0
			}
			if originalSize > 0 {
				logrus.Infof("Revert: scaling nodegroup %s back to %d nodes", ngName, originalSize)
				if err := s.awsOps.ScaleEKSNodeGroup(s.config.EKSCluster, ngName, originalSize); err != nil {
					logrus.Warnf("Revert: failed to scale nodegroup back: %v", err)
				} else {
					logrus.Infof("Revert: successfully initiated scale-back of nodegroup %s to %d", ngName, originalSize)
				}
			}
		}
	}

	// 2. Uncordon nodes that were cordoned
	if cordonedRaw, ok := ctx["cordonedNodes"]; ok && cordonedRaw != nil {
		var nodeNames []string
		switch v := cordonedRaw.(type) {
		case []string:
			nodeNames = v
		case []interface{}:
			for _, e := range v {
				if str, ok := e.(string); ok {
					nodeNames = append(nodeNames, str)
				}
			}
		}
		for _, nodeName := range nodeNames {
			logrus.Infof("Revert: uncordoning node %s", nodeName)
			if err := s.k8sOps.UncordonNode(nodeName); err != nil {
				logrus.Warnf("Revert: failed to uncordon node %s: %v", nodeName, err)
			} else {
				logrus.Infof("Revert: successfully uncordoned node %s", nodeName)
			}
		}
	}

	// 3. If there are pending relay pods, scale the deployment back down to match scheduled pods
	if s.k8sOps != nil {
		pods, err := s.k8sOps.GetRelayPods()
		if err != nil {
			logrus.Warnf("Revert: failed to get relay pods while checking for pending pods: %v", err)
			return
		}

		var scheduledCount int32
		hasPending := false
		for _, pod := range pods {
			if pod.Node == "" || pod.Node == "<none>" {
				hasPending = true
			} else {
				scheduledCount++
			}
		}

		if hasPending && scheduledCount > 0 {
			logrus.Infof("Revert: found pending relay pods; scaling deployment %s down to %d replicas", s.config.Deployment, scheduledCount)
			if err := s.k8sOps.ScaleDeployment(s.config.Deployment, scheduledCount); err != nil {
				logrus.Warnf("Revert: failed to scale relay deployment during rollback: %v", err)
			} else {
				logrus.Infof("Revert: successfully scaled relay deployment %s down to %d replicas", s.config.Deployment, scheduledCount)
			}
		}
	}
}

func (s *SOPService) stepDeleteOldPods() error {
	s.addLog("Deleting old pods and scaling back deployment")

	// Use the stored old pods from previous step
	if len(s.oldPods) == 0 {
		return fmt.Errorf("no old pods found to delete")
	}

	// Extract pod names for parallel deletion
	var podNames []string
	for _, pod := range s.oldPods {
		podNames = append(podNames, pod.Name)
		s.addLog(fmt.Sprintf("Will delete old pod: %s/%s", pod.Namespace, pod.Name))
	}

	// Delete all old pods in parallel using a single kubectl command
	s.addLog(fmt.Sprintf("Deleting %d old pods in parallel", len(podNames)))
	err := s.k8sOps.DeletePods(podNames)
	if err != nil {
		return fmt.Errorf("failed to delete old pods: %v", err)
	}

	s.addLog(fmt.Sprintf("Successfully deleted %d old pods in parallel", len(s.oldPods)))

	// Scale back to original replica count
	currentReplicas, err := s.k8sOps.GetDeploymentReplicas(s.config.Deployment)
	if err != nil {
		return fmt.Errorf("failed to get deployment replicas: %v", err)
	}

	originalReplicas := currentReplicas / 2 // Scale back to original
	s.addLog(fmt.Sprintf("Current deployment replicas: %d", currentReplicas))
	s.addLog(fmt.Sprintf("Target deployment replicas: %d", originalReplicas))

	// Check if already at or below target replicas (idempotency check)
	if currentReplicas <= originalReplicas {
		s.addLog(fmt.Sprintf("Deployment %s is already at or below target replicas (%d <= %d), skipping scaling", s.config.Deployment, currentReplicas, originalReplicas))
	} else {
		err = s.k8sOps.ScaleDeployment(s.config.Deployment, originalReplicas)
		if err != nil {
			return fmt.Errorf("failed to scale back deployment: %v", err)
		}
		s.addLog(fmt.Sprintf("Scaled deployment back to %d replicas", originalReplicas))
	}
	return nil
}

func (s *SOPService) stepRemoveScaleInProtections() error {
	s.addLog("Removing scale in protections")

	// Find the nodegroup and ASG
	nodeGroupName, err := s.awsOps.FindNodeGroup(s.config.EKSCluster, s.config.NodeGroup)
	if err != nil {
		return fmt.Errorf("failed to find nodegroup: %v", err)
	}

	asgName, err := s.awsOps.GetNodeGroupASG(s.config.EKSCluster, *nodeGroupName)
	if err != nil {
		return fmt.Errorf("failed to get ASG for nodegroup: %v", err)
	}

	s.addLog(fmt.Sprintf("Removing scale-in protections for ASG: %s", *asgName))

	// Check current protections first
	protections, err := s.awsOps.CheckScaleInProtections(*asgName)
	if err != nil {
		s.addLog(fmt.Sprintf("Warning: Could not check current protections: %v", err))
	} else {
		s.addLog("Current scale-in protections:")
		for instanceId, protected := range protections {
			s.addLog(fmt.Sprintf("  - %s: %t", instanceId, protected))
		}
	}

	err = s.awsOps.RemoveScaleInProtections(*asgName)
	if err != nil {
		return fmt.Errorf("failed to remove scale in protections: %v", err)
	}

	s.addLog("Successfully removed scale in protections")
	return nil
}

func (s *SOPService) stepAddScaleInProtections() error {
	s.addLog("Adding scale in protections for new nodes")

	// Get new relay pods to find which nodes they're running on
	pods, err := s.k8sOps.GetRelayPods()
	if err != nil {
		return fmt.Errorf("failed to get relay pods: %v", err)
	}

	// Get unique nodes where new relay pods are running
	nodeMap := make(map[string]bool)
	for _, pod := range pods {
		nodeMap[pod.Node] = true
	}

	// Find the nodegroup and ASG
	nodeGroupName, err := s.awsOps.FindNodeGroup(s.config.EKSCluster, s.config.NodeGroup)
	if err != nil {
		return fmt.Errorf("failed to find nodegroup: %v", err)
	}

	asgName, err := s.awsOps.GetNodeGroupASG(s.config.EKSCluster, *nodeGroupName)
	if err != nil {
		return fmt.Errorf("failed to get ASG for nodegroup: %v", err)
	}

	// Get all instances in the ASG for validation
	_, err = s.awsOps.GetASGInstances(*asgName)
	if err != nil {
		return fmt.Errorf("failed to get ASG instances: %v", err)
	}

	// Get node-to-instance mapping by matching private IPs
	s.addLog("Getting node-to-instance mapping...")
	nodeToInstanceMap, err := s.k8sOps.GetNodeToInstanceMapping()
	if err != nil {
		return fmt.Errorf("failed to get node-to-instance mapping: %v", err)
	}
	s.addLog(fmt.Sprintf("Node-to-instance mapping: %v", nodeToInstanceMap))

	// Find instances that correspond to nodes with relay pods
	var instancesToProtect []string
	for nodeName := range nodeMap {
		if instanceId, exists := nodeToInstanceMap[nodeName]; exists {
			instancesToProtect = append(instancesToProtect, instanceId)
			s.addLog(fmt.Sprintf("Node %s maps to instance %s - will protect", nodeName, instanceId))
		} else {
			s.addLog(fmt.Sprintf("Warning: Could not find instance ID for node %s", nodeName))
		}
	}

	// Add protection to instances
	if len(instancesToProtect) > 0 {
		err = s.awsOps.AddScaleInProtections(*asgName, instancesToProtect)
		if err != nil {
			return fmt.Errorf("failed to add scale in protections: %v", err)
		}
		s.addLog(fmt.Sprintf("Added scale in protection to %d instances: %v", len(instancesToProtect), instancesToProtect))
	} else {
		s.addLog("Warning: No instances found to protect")
	}

	return nil
}

func (s *SOPService) addLog(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Logs = append(s.status.Logs, fmt.Sprintf("[%s] %s", time.Now().Format("2006-01-02 15:04:05"), message))
}

func (s *SOPService) GetStatus() SOPStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// resumeExecution resumes execution from a saved state
func (s *SOPService) resumeExecution(state *SOPExecutionState) error {
	s.addLog(fmt.Sprintf("Resuming execution from step %d", state.CurrentStep))

	// Restore context data
	if oldPodsData, ok := state.Context["oldPods"]; ok {
		if pods, ok := oldPodsData.([]PodInfo); ok {
			s.oldPods = pods
		}
	}
	if oldNodesData, ok := state.Context["oldNodes"]; ok {
		if nodes, ok := oldNodesData.([]string); ok {
			s.oldNodes = nodes
		}
	}

	// Find the last successfully completed step
	lastCompletedStep := -1
	for stepNum, stepResult := range state.StepResults {
		if stepResult.Status == "Completed" {
			lastCompletedStep = stepNum
		}
	}

	// Determine the resume step: start from the next step after the last completed one
	resumeStep := lastCompletedStep + 1
	if resumeStep < 0 {
		resumeStep = 0 // Start from beginning if no steps completed
	}

	s.addLog(fmt.Sprintf("Last completed step: %d, resuming from step: %d", lastCompletedStep, resumeStep))

	// Set up the status for resumed execution
	s.mu.Lock()
	s.status = SOPStatus{
		Status:    "Running",
		StartTime: state.StartTime,
		Logs:      []string{fmt.Sprintf("Resumed execution from step %d (last completed: %d) at %s", resumeStep, lastCompletedStep, time.Now().Format("2006-01-02 15:04:05"))},
	}
	s.mu.Unlock()

	// Continue execution from the resume step
	return s.continueExecutionFromStep(resumeStep)
}

// executeStepWithRetry executes a step with retry logic
func (s *SOPService) executeStepWithRetry(stepFn func() error, stepIndex int) error {
	maxRetries := 3
	baseDelay := 5 * time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * baseDelay
			s.addLog(fmt.Sprintf("Retry attempt %d/%d after %v delay", attempt, maxRetries, delay))
			time.Sleep(delay)
		}

		if err := stepFn(); err != nil {
			if attempt == maxRetries {
				return fmt.Errorf("step failed after %d retries: %v", maxRetries, err)
			}
			s.addLog(fmt.Sprintf("Step failed (attempt %d/%d): %v", attempt+1, maxRetries+1, err))
			continue
		}

		// Step succeeded
		if attempt > 0 {
			s.addLog(fmt.Sprintf("Step succeeded on retry attempt %d", attempt+1))
		}
		return nil
	}

	return fmt.Errorf("step failed after all retry attempts")
}

// continueExecutionFromStep continues execution from a specific step
func (s *SOPService) continueExecutionFromStep(startStep int) error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"Get AWS region and EKS cluster from configmap", s.stepGetConfig},
		{"Find nodegroup with label Name=nodegroup-relay-services and nodecount >1", s.stepFindNodeGroup},
		{"Get list of nodes with old relay pods", s.stepGetOldRelayNodes},
		{"Cordon nodes with old relay pods", s.stepCordonNodes},
		{"Scale nodegroup to 2X current count", s.stepScaleNodeGroup},
		{"Wait for new nodes to join cluster", s.stepWaitForNewNodes},
		{"Rollout restart rafay-sentry deployment", s.stepRestartSentryDeployment},
		{"Scale rafay-relay deployment to 2x", s.stepScaleRelayDeployment},
		{"Wait for new pods to be running", s.stepWaitForNewPods},
		{"Delete old pods and scale back deployment", s.stepDeleteOldPods},
		{"Remove scale in protections", s.stepRemoveScaleInProtections},
		{"Add scale in protection for new nodes", s.stepAddScaleInProtections},
		{"Scale nodegroup back to original size", s.stepScaleNodeGroupBack},
	}

	// Start from the specified step
	for i := startStep; i < len(steps); i++ {
		s.addLog(fmt.Sprintf("Processing step %d: %s", i, steps[i].name))

		// Check if step should be skipped (already completed)
		if s.stateManager != nil {
			state := s.stateManager.GetCurrentState()
			if state != nil && state.StepResults[i].Status == "Completed" {
				s.addLog(fmt.Sprintf("Skipping completed step %d: %s", i, steps[i].name))
				continue
			}
		}

		// Start the step
		if s.stateManager != nil {
			if err := s.stateManager.StartStep(i, steps[i].name); err != nil {
				s.addLog(fmt.Sprintf("Warning: Failed to start step tracking: %v", err))
			}
		}

		// Execute the step with retry logic
		s.addLog(fmt.Sprintf("Executing step %d: %s", i, steps[i].name))
		if err := s.executeStepWithRetry(steps[i].fn, i); err != nil {
			// Mark step as failed
			if s.stateManager != nil {
				if err2 := s.stateManager.FailStep(i, err.Error()); err2 != nil {
					s.addLog(fmt.Sprintf("Warning: Failed to mark step as failed: %v", err2))
				}
			}
			s.addLog(fmt.Sprintf("Step %d (%s) failed: %v", i, steps[i].name, err))
			return fmt.Errorf("step %d (%s) failed: %v", i, steps[i].name, err)
		}

		// Mark step as completed
		if s.stateManager != nil {
			if err := s.stateManager.CompleteStep(i); err != nil {
				s.addLog(fmt.Sprintf("Warning: Failed to mark step as completed: %v", err))
			}
		}

		s.addLog(fmt.Sprintf("Successfully completed step %d: %s", i, steps[i].name))
	}

	return nil
}
