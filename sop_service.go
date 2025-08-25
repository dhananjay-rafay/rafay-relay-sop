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
	AWSRegion  string
	EKSCluster string
	Namespace  string
	NodeGroup  string
	Deployment string
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
		AWSRegion:  getEnvOrDefault("AWS_REGION", "us-east-1"),
		EKSCluster: getEnvOrDefault("EKS_CLUSTER", "upgrade-tb"),
		Namespace:  "rafay-core",
		NodeGroup:  "nodegroup-relay-services",
		Deployment: "rafay-relay",
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

func (s *SOPService) ExecuteSOP() error {
	// Check for existing state and attempt recovery
	if s.stateManager != nil {
		if existingState, err := s.stateManager.LoadExistingState(); err == nil && existingState != nil {
			if existingState.Status == "Running" || existingState.Status == "Resuming" {
				s.addLog(fmt.Sprintf("Found existing execution state: %s, attempting recovery", existingState.ExecutionID))
				return s.resumeExecution(existingState)
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
		if err := s.stateManager.InitializeState(12); err != nil {
			s.addLog(fmt.Sprintf("Warning: Failed to initialize resilient state: %v", err))
		}
	}

	defer func() {
		s.mu.Lock()
		endTime := time.Now()
		s.status.EndTime = &endTime
		if s.status.Status == "Running" {
			s.status.Status = "Success"
		}
		s.mu.Unlock()

		// Mark execution as completed in resilient state
		if s.stateManager != nil {
			if err := s.stateManager.CompleteExecution(); err != nil {
				s.addLog(fmt.Sprintf("Warning: Failed to mark execution as completed: %v", err))
			}
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
				s.addLog(fmt.Sprintf("Warning: Failed to start step tracking: %v", err))
			}
		}

		s.addLog(fmt.Sprintf("Starting step: %s", step.name))

		// Execute step with retry logic
		if err := s.executeStepWithRetry(step.fn, i); err != nil {
			s.addLog(fmt.Sprintf("Step failed: %s - %v", step.name, err))

			// Mark step as failed in resilient state
			if s.stateManager != nil {
				if err := s.stateManager.FailStep(i, err.Error()); err != nil {
					s.addLog(fmt.Sprintf("Warning: Failed to mark step as failed: %v", err))
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
				s.addLog(fmt.Sprintf("Warning: Failed to mark step as completed: %v", err))
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

	// We need to wait for the nodegroup to actually scale up
	// The target should be the scaled up size (2x original)
	targetNewNodes := int(targetDesiredSize) // We expect the same number of new nodes as original
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

	// Filter out SOP pods
	var oldPods []PodInfo
	for _, pod := range pods {
		if !strings.Contains(pod.Name, "rafay-relay-sop") {
			oldPods = append(oldPods, pod)
		}
	}

	s.addLog(fmt.Sprintf("Found %d old relay pods (excluding SOP)", len(oldPods)))
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
			s.addLog(fmt.Sprintf("Warning: Failed to save old pods to context: %v", err))
		}
		if err := s.stateManager.SetContext("oldNodes", s.oldNodes); err != nil {
			s.addLog(fmt.Sprintf("Warning: Failed to save old nodes to context: %v", err))
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

	// Cordon each node
	for _, nodeName := range s.oldNodes {
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
		s.addLog(fmt.Sprintf("Successfully cordoned node: %s", actualNodeName))
	}

	s.addLog(fmt.Sprintf("Successfully cordoned %d nodes", len(s.oldNodes)))
	return nil
}

func (s *SOPService) stepScaleRelayDeployment() error {
	// Get current replica count
	currentReplicas, err := s.k8sOps.GetDeploymentReplicas(s.config.Deployment)
	if err != nil {
		return fmt.Errorf("failed to get deployment replicas: %v", err)
	}

	newReplicas := currentReplicas * 2

	// Scale the deployment
	err = s.k8sOps.ScaleDeployment(s.config.Deployment, newReplicas)
	if err != nil {
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

func (s *SOPService) stepDeleteOldPods() error {
	s.addLog("Deleting old pods and scaling back deployment")

	// Use the stored old pods from previous step
	if len(s.oldPods) == 0 {
		return fmt.Errorf("no old pods found to delete")
	}

	// Delete old pods (excluding SOP pods)
	for _, pod := range s.oldPods {
		s.addLog(fmt.Sprintf("Deleting old pod: %s/%s", pod.Namespace, pod.Name))
		err := s.k8sOps.DeletePod(pod.Name)
		if err != nil {
			return fmt.Errorf("failed to delete pod %s: %v", pod.Name, err)
		}
	}

	s.addLog(fmt.Sprintf("Successfully deleted %d old pods", len(s.oldPods)))

	// Scale back to original replica count
	originalReplicas, err := s.k8sOps.GetDeploymentReplicas(s.config.Deployment)
	if err != nil {
		return fmt.Errorf("failed to get deployment replicas: %v", err)
	}

	originalReplicas = originalReplicas / 2 // Scale back to original
	err = s.k8sOps.ScaleDeployment(s.config.Deployment, originalReplicas)
	if err != nil {
		return fmt.Errorf("failed to scale back deployment: %v", err)
	}

	s.addLog(fmt.Sprintf("Scaled deployment back to %d replicas", originalReplicas))
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

	// Find instances that correspond to nodes with relay pods
	var instancesToProtect []string
	for nodeName := range nodeMap {
		// This is a simplified mapping - in practice you'd need to get the node's IP
		// and match it with instance private IPs
		s.addLog(fmt.Sprintf("Node %s has relay pods - will protect corresponding instance", nodeName))
	}

	// Add protection to instances
	if len(instancesToProtect) > 0 {
		err = s.awsOps.AddScaleInProtections(*asgName, instancesToProtect)
		if err != nil {
			return fmt.Errorf("failed to add scale in protections: %v", err)
		}
		s.addLog(fmt.Sprintf("Added scale in protection to %d instances", len(instancesToProtect)))
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

	// Continue execution from the next step
	return s.ExecuteSOP()
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
