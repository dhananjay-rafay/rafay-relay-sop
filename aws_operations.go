package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
)

type AWSOperations struct {
	eksClient *eks.Client
	asgClient *autoscaling.Client
	ec2Client *ec2.Client
	region    string
}

func NewAWSOperations(eksClient *eks.Client, asgClient *autoscaling.Client, ec2Client *ec2.Client, region string) *AWSOperations {
	return &AWSOperations{
		eksClient: eksClient,
		asgClient: asgClient,
		ec2Client: ec2Client,
		region:    region,
	}
}

func (a *AWSOperations) FindNodeGroup(clusterName, nodeGroupLabel string) (*string, error) {
	// List nodegroups for the cluster
	input := &eks.ListNodegroupsInput{
		ClusterName: aws.String(clusterName),
	}

	result, err := a.eksClient.ListNodegroups(context.TODO(), input)
	if err != nil {
		return nil, fmt.Errorf("failed to list nodegroups: %v", err)
	}

	// Find nodegroup with the specified label
	for _, nodegroupName := range result.Nodegroups {
		describeInput := &eks.DescribeNodegroupInput{
			ClusterName:   aws.String(clusterName),
			NodegroupName: aws.String(nodegroupName),
		}

		nodegroup, err := a.eksClient.DescribeNodegroup(context.TODO(), describeInput)
		if err != nil {
			continue
		}

		// Check if nodegroup has the required label and count > 1
		if nodegroup.Nodegroup.Labels != nil {
			if labelValue, exists := nodegroup.Nodegroup.Labels["Name"]; exists && labelValue == nodeGroupLabel {
				if nodegroup.Nodegroup.ScalingConfig != nil && *nodegroup.Nodegroup.ScalingConfig.DesiredSize > 1 {
					return aws.String(nodegroupName), nil
				}
			}
		}
	}

	return nil, fmt.Errorf("no nodegroup found with label Name=%s and count > 1", nodeGroupLabel)
}

func (a *AWSOperations) GetNodeGroupASG(clusterName, nodeGroupName string) (*string, error) {
	// Get nodegroup details to find the Auto Scaling Group
	describeInput := &eks.DescribeNodegroupInput{
		ClusterName:   aws.String(clusterName),
		NodegroupName: aws.String(nodeGroupName),
	}

	nodegroup, err := a.eksClient.DescribeNodegroup(context.TODO(), describeInput)
	if err != nil {
		return nil, fmt.Errorf("failed to describe nodegroup: %v", err)
	}

	// Extract ASG name from nodegroup resources
	if nodegroup.Nodegroup.Resources != nil && len(nodegroup.Nodegroup.Resources.AutoScalingGroups) > 0 {
		return nodegroup.Nodegroup.Resources.AutoScalingGroups[0].Name, nil
	}

	return nil, fmt.Errorf("no Auto Scaling Group found for nodegroup %s", nodeGroupName)
}

func (a *AWSOperations) ScaleNodeGroup(asgName string, targetCount int32) error {
	// Use AWS CLI instead of SDK to avoid endpoint resolution issues
	cmd := exec.Command("aws", "autoscaling", "update-auto-scaling-group",
		"--auto-scaling-group-name", asgName,
		"--desired-capacity", fmt.Sprintf("%d", targetCount),
		"--min-size", fmt.Sprintf("%d", targetCount),
		"--max-size", fmt.Sprintf("%d", targetCount*2), // Allow some headroom
		"--region", a.region)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to scale nodegroup using AWS CLI: %v, output: %s", err, string(output))
	}

	return nil
}

func (a *AWSOperations) GetASGInstances(asgName string) ([]string, error) {
	// Use AWS CLI instead of SDK to avoid endpoint resolution issues
	cmd := exec.Command("aws", "autoscaling", "describe-auto-scaling-groups",
		"--auto-scaling-group-names", asgName,
		"--region", a.region,
		"--query", "AutoScalingGroups[0].Instances[*].InstanceId",
		"--output", "text")

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to describe ASG using AWS CLI: %v", err)
	}

	// Parse the output
	instanceIds := strings.Fields(string(output))
	if len(instanceIds) == 0 {
		return nil, fmt.Errorf("ASG %s not found or has no instances", asgName)
	}

	return instanceIds, nil
}

func (a *AWSOperations) CheckScaleInProtections(asgName string) (map[string]bool, error) {
	instanceIds, err := a.GetASGInstances(asgName)
	if err != nil {
		return nil, err
	}

	protections := make(map[string]bool)
	for _, instanceId := range instanceIds {
		cmd := exec.Command("aws", "autoscaling", "describe-auto-scaling-instances",
			"--instance-ids", instanceId,
			"--region", a.region,
			"--query", "AutoScalingInstances[0].ProtectedFromScaleIn",
			"--output", "text")

		output, err := cmd.Output()
		if err != nil {
			continue
		}

		protected := strings.TrimSpace(string(output)) == "True"
		protections[instanceId] = protected
	}

	return protections, nil
}

func (a *AWSOperations) RemoveScaleInProtections(asgName string) error {
	instanceIds, err := a.GetASGInstances(asgName)
	if err != nil {
		return err
	}

	for _, instanceId := range instanceIds {
		cmd := exec.Command("aws", "autoscaling", "set-instance-protection",
			"--auto-scaling-group-name", asgName,
			"--instance-ids", instanceId,
			"--protected-from-scale-in", "false",
			"--region", a.region)

		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to remove scale in protection for instance %s: %v, output: %s", instanceId, err, string(output))
		}
	}

	return nil
}

func (a *AWSOperations) AddScaleInProtections(asgName string, instanceIds []string) error {
	for _, instanceId := range instanceIds {
		cmd := exec.Command("aws", "autoscaling", "set-instance-protection",
			"--auto-scaling-group-name", asgName,
			"--instance-ids", instanceId,
			"--protected-from-scale-in", "true",
			"--region", a.region)

		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to add scale in protection for instance %s: %v, output: %s", instanceId, err, string(output))
		}
	}

	return nil
}

func (a *AWSOperations) GetInstancePrivateIPs(instanceIds []string) (map[string]string, error) {
	input := &ec2.DescribeInstancesInput{
		InstanceIds: instanceIds,
	}

	result, err := a.ec2Client.DescribeInstances(context.TODO(), input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %v", err)
	}

	ipMap := make(map[string]string)
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			if instance.PrivateIpAddress != nil {
				ipMap[*instance.InstanceId] = *instance.PrivateIpAddress
			}
		}
	}

	return ipMap, nil
}
