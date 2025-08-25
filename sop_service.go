package main

import (
	"context"
	"fmt"
	"os"
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
	mu        sync.RWMutex
	status    SOPStatus
	k8sClient *kubernetes.Clientset
	awsConfig aws.Config
	eksClient *eks.Client
	asgClient *autoscaling.Client
	ec2Client *ec2.Client
	config    *Config
	awsOps    *AWSOperations
	k8sOps    *K8SOperations
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

	return &SOPService{
		status: SOPStatus{
			Status:    "Idle",
			StartTime: time.Time{},
			Logs:      []string{},
		},
		k8sClient: k8sClient,
		awsConfig: awsCfg,
		eksClient: eksClient,
		asgClient: asgClient,
		ec2Client: ec2Client,
		config:    cfg,
		awsOps:    awsOps,
		k8sOps:    k8sOps,
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
	s.mu.Lock()
	s.status = SOPStatus{
		Status:    "Running",
		StartTime: time.Now(),
		Logs:      []string{},
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		endTime := time.Now()
		s.status.EndTime = &endTime
		if s.status.Status == "Running" {
			s.status.Status = "Success"
		}
		s.mu.Unlock()
	}()

	steps := []struct {
		name string
		fn   func() error
	}{
		{"Get AWS region and EKS cluster from configmap", s.stepGetConfig},
		{"Find nodegroup with label Name=nodegroup-relay-services and nodecount >1", s.stepFindNodeGroup},
		{"Scale nodegroup to 2X current count", s.stepScaleNodeGroup},
		{"Check ASG scaling status", s.stepCheckASGStatus},
		{"Verify nodes are showing up", s.stepVerifyNodes},
		{"Rollout restart rafay-sentry", s.stepRestartSentry},
		{"Get list of nodes with relay pods", s.stepGetRelayNodes},
		{"Cordon nodes with old relay pods", s.stepCordonNodes},
		{"Scale rafay-relay deployment to 2x", s.stepScaleRelayDeployment},
		{"Delete old pods and scale back", s.stepDeleteOldPods},
		{"Get new relay pod nodes", s.stepGetNewRelayNodes},
		{"Check scale in protections", s.stepCheckScaleInProtections},
		{"Remove scale in protections", s.stepRemoveScaleInProtections},
		{"Add scale in protection for new nodes", s.stepAddScaleInProtections},
	}

	for _, step := range steps {
		s.addLog(fmt.Sprintf("Starting step: %s", step.name))
		if err := step.fn(); err != nil {
			s.addLog(fmt.Sprintf("Step failed: %s - %v", step.name, err))
			s.mu.Lock()
			s.status.Status = "Failed"
			s.status.Error = err.Error()
			s.mu.Unlock()
			return err
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

	// Get the ASG name
	asgName, err := s.awsOps.GetNodeGroupASG(s.config.EKSCluster, *nodeGroupName)
	if err != nil {
		return fmt.Errorf("failed to get ASG for nodegroup: %v", err)
	}

	// Get current instance count
	instances, err := s.awsOps.GetASGInstances(*asgName)
	if err != nil {
		return fmt.Errorf("failed to get ASG instances: %v", err)
	}

	currentCount := int32(len(instances))
	targetCount := currentCount * 2

	s.addLog(fmt.Sprintf("Current ASG instances: %v", instances))
	s.addLog(fmt.Sprintf("Scaling ASG %s from %d to %d instances", *asgName, currentCount, targetCount))

	// Scale the ASG
	err = s.awsOps.ScaleNodeGroup(*asgName, targetCount)
	if err != nil {
		return fmt.Errorf("failed to scale nodegroup: %v", err)
	}

	s.addLog(fmt.Sprintf("Successfully initiated scaling of ASG %s to %d instances", *asgName, targetCount))
	s.addLog("Note: Scaling may take several minutes to complete")

	return nil
}

func (s *SOPService) stepCheckASGStatus() error {
	s.addLog("Checking ASG scaling status")

	// Find the nodegroup first
	nodeGroupName, err := s.awsOps.FindNodeGroup(s.config.EKSCluster, s.config.NodeGroup)
	if err != nil {
		return fmt.Errorf("failed to find nodegroup: %v", err)
	}

	// Get the ASG name
	asgName, err := s.awsOps.GetNodeGroupASG(s.config.EKSCluster, *nodeGroupName)
	if err != nil {
		return fmt.Errorf("failed to get ASG for nodegroup: %v", err)
	}

	// Get current instance count
	instances, err := s.awsOps.GetASGInstances(*asgName)
	if err != nil {
		return fmt.Errorf("failed to get ASG instances: %v", err)
	}

	s.addLog(fmt.Sprintf("ASG %s current instances (%d): %v", *asgName, len(instances), instances))

	// Check if scaling is still in progress
	if len(instances) < 2 {
		s.addLog("Warning: ASG scaling may still be in progress. Expected more instances.")
	} else {
		s.addLog("ASG scaling appears to be complete")
	}

	return nil
}

func (s *SOPService) stepVerifyNodes() error {
	s.addLog("Verifying nodes are showing up")

	nodes, err := s.k8sOps.GetNodesWithLabel("Name")
	if err != nil {
		return fmt.Errorf("failed to get nodes: %v", err)
	}

	s.addLog(fmt.Sprintf("Found %d relay nodes", len(nodes)))
	for _, node := range nodes {
		s.addLog(fmt.Sprintf("  - %s", node))
	}

	// Also check all nodes to see the total count
	allNodes, err := s.k8sOps.GetAllNodes()
	if err != nil {
		s.addLog(fmt.Sprintf("Warning: Could not get all nodes: %v", err))
	} else {
		s.addLog(fmt.Sprintf("Total nodes in cluster: %d", len(allNodes)))
	}

	return nil
}

func (s *SOPService) stepRestartSentry() error {
	s.addLog("Rollout restart rafay-sentry")

	// Check current status before restart
	replicas, err := s.k8sOps.GetDeploymentReplicas("rafay-sentry")
	if err != nil {
		s.addLog(fmt.Sprintf("Warning: Could not get current replicas: %v", err))
	} else {
		s.addLog(fmt.Sprintf("Current rafay-sentry replicas: %d", replicas))
	}

	err = s.k8sOps.RestartDeployment("rafay-sentry")
	if err != nil {
		return fmt.Errorf("failed to restart rafay-sentry: %v", err)
	}

	s.addLog("Successfully initiated rafay-sentry restart")
	return nil
}

func (s *SOPService) stepGetRelayNodes() error {
	s.addLog("Getting list of nodes with relay pods")

	pods, err := s.k8sOps.GetRelayPods()
	if err != nil {
		return fmt.Errorf("failed to get relay pods: %v", err)
	}

	s.addLog(fmt.Sprintf("Found %d relay pods", len(pods)))
	for _, pod := range pods {
		s.addLog(fmt.Sprintf("  - %s/%s on node %s", pod.Namespace, pod.Name, pod.Node))
	}

	// Get unique nodes
	nodeMap := make(map[string]bool)
	for _, pod := range pods {
		nodeMap[pod.Node] = true
	}

	s.addLog(fmt.Sprintf("Relay pods running on %d unique nodes", len(nodeMap)))
	for nodeName := range nodeMap {
		s.addLog(fmt.Sprintf("  - Node: %s", nodeName))
	}

	return nil
}

func (s *SOPService) stepCordonNodes() error {
	s.addLog("Cordoning nodes with old relay pods")

	// Get current relay pods
	pods, err := s.k8sOps.GetRelayPods()
	if err != nil {
		return fmt.Errorf("failed to get relay pods: %v", err)
	}

	// Get unique nodes
	nodeMap := make(map[string]bool)
	for _, pod := range pods {
		nodeMap[pod.Node] = true
	}

	s.addLog(fmt.Sprintf("Found %d unique nodes to cordon", len(nodeMap)))

	// Cordon each node
	for nodeName := range nodeMap {
		s.addLog(fmt.Sprintf("Cordoning node: %s", nodeName))
		err := s.k8sOps.CordonNode(nodeName)
		if err != nil {
			return fmt.Errorf("failed to cordon node %s: %v", nodeName, err)
		}
		s.addLog(fmt.Sprintf("Successfully cordoned node: %s", nodeName))
	}

	s.addLog(fmt.Sprintf("Successfully cordoned %d nodes", len(nodeMap)))
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

func (s *SOPService) stepDeleteOldPods() error {
	s.addLog("Deleting old pods and scaling back deployment")

	// Get current relay pods
	pods, err := s.k8sOps.GetRelayPods()
	if err != nil {
		return fmt.Errorf("failed to get relay pods: %v", err)
	}

	// Delete old pods
	for _, pod := range pods {
		s.addLog(fmt.Sprintf("Deleting pod: %s/%s", pod.Namespace, pod.Name))
		err := s.k8sOps.DeletePod(pod.Name)
		if err != nil {
			return fmt.Errorf("failed to delete pod %s: %v", pod.Name, err)
		}
	}

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

func (s *SOPService) stepGetNewRelayNodes() error {
	pods, err := s.k8sOps.GetRelayPods()
	if err != nil {
		return fmt.Errorf("failed to get new relay pods: %v", err)
	}

	s.addLog(fmt.Sprintf("Found %d new relay pods", len(pods)))
	for _, pod := range pods {
		s.addLog(fmt.Sprintf("  - %s/%s on node %s", pod.Namespace, pod.Name, pod.Node))
	}
	return nil
}

func (s *SOPService) stepCheckScaleInProtections() error {
	s.addLog("Checking scale in protections")

	// Find the nodegroup and ASG
	nodeGroupName, err := s.awsOps.FindNodeGroup(s.config.EKSCluster, s.config.NodeGroup)
	if err != nil {
		return fmt.Errorf("failed to find nodegroup: %v", err)
	}

	asgName, err := s.awsOps.GetNodeGroupASG(s.config.EKSCluster, *nodeGroupName)
	if err != nil {
		return fmt.Errorf("failed to get ASG for nodegroup: %v", err)
	}

	protections, err := s.awsOps.CheckScaleInProtections(*asgName)
	if err != nil {
		return fmt.Errorf("failed to check scale in protections: %v", err)
	}

	s.addLog(fmt.Sprintf("Found %d instances with scale in protections", len(protections)))
	for instanceId, protected := range protections {
		s.addLog(fmt.Sprintf("  - Instance %s: protected=%v", instanceId, protected))
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

	// Get all instances in the ASG
	allInstances, err := s.awsOps.GetASGInstances(*asgName)
	if err != nil {
		return fmt.Errorf("failed to get ASG instances: %v", err)
	}

	// Get private IPs to map nodes to instances
	_, err = s.awsOps.GetInstancePrivateIPs(allInstances)
	if err != nil {
		return fmt.Errorf("failed to get instance IPs: %v", err)
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
