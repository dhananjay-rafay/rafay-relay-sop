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

func (k *K8SOperations) GetNodesWithLabel(label string) ([]string, error) {
	cmd := exec.Command("kubectl", "get", "nodes", "-L", label)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get nodes: %v", err)
	}

	var nodes []string
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "relay") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				nodes = append(nodes, fields[0])
			}
		}
	}

	return nodes, nil
}

func (k *K8SOperations) GetRelayPods() ([]PodInfo, error) {
	cmd := exec.Command("kubectl", "get", "po", "-A", "-owide")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get pods: %v", err)
	}

	var pods []PodInfo
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "rafay-relay") {
			fields := strings.Fields(line)
			if len(fields) >= 7 {
				pod := PodInfo{
					Namespace: fields[0],
					Name:      fields[1],
					Node:      fields[6],
				}
				pods = append(pods, pod)
			}
		}
	}

	return pods, nil
}

func (k *K8SOperations) CordonNode(nodeName string) error {
	cmd := exec.Command("kubectl", "cordon", nodeName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to cordon node %s: %v", nodeName, err)
	}
	return nil
}

func (k *K8SOperations) UncordonNode(nodeName string) error {
	cmd := exec.Command("kubectl", "uncordon", nodeName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to uncordon node %s: %v", nodeName, err)
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

func (k *K8SOperations) GetPodsOnNode(nodeName string) ([]PodInfo, error) {
	cmd := exec.Command("kubectl", "get", "po", "-A", "-owide", "--field-selector=spec.nodeName="+nodeName)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get pods on node %s: %v", nodeName, err)
	}

	var pods []PodInfo
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "NAMESPACE") {
			fields := strings.Fields(line)
			if len(fields) >= 7 {
				pod := PodInfo{
					Namespace: fields[0],
					Name:      fields[1],
					Node:      fields[6],
				}
				pods = append(pods, pod)
			}
		}
	}

	return pods, nil
}

type PodInfo struct {
	Namespace string
	Name      string
	Node      string
}
