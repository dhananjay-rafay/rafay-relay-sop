package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type K8SOperations struct {
	client    *kubernetes.Clientset
	namespace string
}

func NewK8SOperations(client *kubernetes.Clientset, namespace string) *K8SOperations {
	return &K8SOperations{
		client:    client,
		namespace: namespace,
	}
}

func (k *K8SOperations) GetAllNodes() ([]string, error) {
	cmd := exec.Command("kubectl", "get", "nodes", "--no-headers", "-o", "custom-columns=NAME:.metadata.name")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get all nodes: %v", err)
	}

	var nodes []string
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nodes = append(nodes, strings.TrimSpace(line))
		}
	}

	return nodes, nil
}

type NodeInfo struct {
	Name   string
	Status string
}

func (k *K8SOperations) GetRelayNodes() ([]NodeInfo, error) {
	cmd := exec.Command("kubectl", "get", "nodes", "-L", "Name", "--no-headers")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get relay nodes: %v", err)
	}

	var nodes []NodeInfo
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "relay") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				node := NodeInfo{
					Name:   fields[0],
					Status: fields[1],
				}
				nodes = append(nodes, node)
			}
		}
	}

	return nodes, nil
}

func (k *K8SOperations) GetRelayPods() ([]PodInfo, error) {
	cmd := exec.Command("kubectl", "get", "po", "-A", "-o", "custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,NODE:.spec.nodeName")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get pods: %v", err)
	}

	var pods []PodInfo
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "rafay-relay") && !strings.Contains(line, "rafay-relay-sop") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				pod := PodInfo{
					Namespace: fields[0],
					Name:      fields[1],
					Node:      fields[2],
				}
				pods = append(pods, pod)
			}
		}
	}

	return pods, nil
}

func (k *K8SOperations) GetNodeNameByIP(ipAddress string) (string, error) {
	cmd := exec.Command("kubectl", "get", "nodes", "-o", "custom-columns=NAME:.metadata.name,IP:.status.addresses[?(@.type==\"InternalIP\")].address")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get nodes: %v", err)
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, ipAddress) {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[0], nil
			}
		}
	}

	return "", fmt.Errorf("node with IP %s not found", ipAddress)
}

func (k *K8SOperations) CordonNode(nodeName string) error {
	cmd := exec.Command("kubectl", "cordon", nodeName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to cordon node %s: %v", nodeName, err)
	}
	return nil
}

func (k *K8SOperations) ScaleDeployment(deploymentName string, replicas int32) error {
	deployment, err := k.client.AppsV1().Deployments(k.namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment: %v", err)
	}

	deployment.Spec.Replicas = &replicas
	_, err = k.client.AppsV1().Deployments(k.namespace).Update(context.TODO(), deployment, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to scale deployment: %v", err)
	}

	return nil
}

func (k *K8SOperations) GetDeploymentReplicas(deploymentName string) (int32, error) {
	deployment, err := k.client.AppsV1().Deployments(k.namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to get deployment: %v", err)
	}

	return *deployment.Spec.Replicas, nil
}

func (k *K8SOperations) DeletePod(podName string) error {
	cmd := exec.Command("kubectl", "delete", "pod", podName, "-n", k.namespace)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to delete pod %s: %v", podName, err)
	}
	return nil
}

func (k *K8SOperations) RestartDeployment(deploymentName string) error {
	cmd := exec.Command("kubectl", "rollout", "restart", "deployment/"+deploymentName, "-n", k.namespace)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart deployment %s: %v", deploymentName, err)
	}
	return nil
}

func (k *K8SOperations) WaitForDeploymentReady(deploymentName string, timeout time.Duration) error {
	cmd := exec.Command("kubectl", "rollout", "status", "deployment/"+deploymentName, "-n", k.namespace, "--timeout="+timeout.String())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("deployment %s not ready within timeout: %v", deploymentName, err)
	}
	return nil
}

type PodInfo struct {
	Namespace string
	Name      string
	Node      string
}
