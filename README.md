# Rafay Relay SOP Service

A Kubernetes service that automates the Standard Operating Procedure (SOP) for scaling and managing Rafay Relay deployments in EKS clusters.

## Overview

This service implements a complete SOP for scaling Rafay Relay deployments with the following steps:

1. **Configuration**: Get AWS region and EKS cluster from configmap
2. **Node Group Discovery**: Find nodegroup with label `Name=nodegroup-relay-services` and nodecount > 1
3. **Scale Node Group**: Scale the nodegroup to 2X its current count
4. **Verify Nodes**: Check that new nodes are showing up and node count matches
5. **Restart Sentry**: Rollout restart rafay-sentry deployment
6. **Get Relay Nodes**: Get list of nodes on which relay pods are running
7. **Cordon Old Nodes**: Cordon nodes on which the old relay pods are running
8. **Scale Relay Deployment**: Scale rafay-relay deployment to 2x its current replica count
9. **Delete Old Pods**: Delete old pods so they end up in pending state, then scale back deployment
10. **Get New Relay Nodes**: Get list of nodes on which new relay pods are running
11. **Check Scale In Protections**: Check if there are any scale in protections in place
12. **Remove Scale In Protections**: Remove existing scale in protections
13. **Add Scale In Protections**: Add scale in protection for instances on which relay pods are running

## API Endpoints

### Start SOP Execution

```bash
curl -XGET http://rafay-relay-sop.rafaycore:8080/run
```

**Response:**

```json
{
  "status": "started",
  "message": "SOP execution started",
  "time": "2024-01-01T12:00:00Z"
}
```

### Get Execution Status

```bash
curl -XGET http://rafay-relay-sop.rafaycore:8080/status
```

**Response:**

```json
{
  "status": "Running|Success|Failed",
  "start_time": "2024-01-01T12:00:00Z",
  "end_time": "2024-01-01T12:05:00Z",
  "logs": [
    "[2024-01-01 12:00:01] Starting step: Get AWS region and EKS cluster from configmap",
    "[2024-01-01 12:00:02] AWS Region: us-west-2, EKS Cluster: rafay-cluster",
    "[2024-01-01 12:00:03] Completed step: Get AWS region and EKS cluster from configmap"
  ],
  "error": "Error message if failed"
}
```

### Health Check

```bash
curl -XGET http://rafay-relay-sop.rafaycore:8080/health
```

**Response:**

```json
{
  "status": "healthy"
}
```

## Prerequisites

- Kubernetes cluster with EKS
- AWS credentials configured (via IAM roles or environment variables)
- kubectl access to the cluster
- Rafay Relay deployment running in `rafay-core` namespace
- eksctl (for IAM role setup)
- AWS CLI configured with appropriate permissions

## Installation

### 1. Build the Docker Image

#### Single Architecture Build

```bash
docker build -t rafay-relay-sop:latest .
```

#### Multi-Architecture Build (AMD64 + ARM64)

```bash
# Using Makefile
make docker-buildx

# Build and push to registry
make docker-buildx-push

# Or manually with Docker Buildx
docker buildx build --platform linux/amd64,linux/arm64 -t rafay-relay-sop:latest .
```

### 2. Setup IAM Role for Service Account (IRSA)

The application uses IAM Roles for Service Accounts to securely access AWS services. This allows the pod to assume an IAM role and perform EKS and Auto Scaling operations.

#### Option A: Using Makefile (Recommended)

```bash
# Setup IAM role and OIDC provider
make setup-iam CLUSTER_NAME=your-cluster-name REGION=us-west-2 ACCOUNT_ID=123456789012

# Deploy with IAM role
make deploy-with-iam ACCOUNT_ID=123456789012
```

#### Option B: Manual Setup

```bash
# 1. Enable OIDC provider for your cluster
eksctl utils associate-iam-oidc-provider --cluster your-cluster-name --region us-west-2 --approve

# 2. Create IAM policy
aws iam create-policy \
    --policy-name rafay-relay-sop-role-policy \
    --policy-document file://k8s/iam-policy.json

# 3. Create IAM role with trust policy
aws iam create-role \
    --role-name rafay-relay-sop-role \
    --assume-role-policy-document file://k8s/iam-trust-policy.json

# 4. Attach policy to role
aws iam attach-role-policy \
    --role-name rafay-relay-sop-role \
    --policy-arn arn:aws:iam::123456789012:policy/rafay-relay-sop-role-policy

# 5. Update deployment with your account ID
sed 's/ACCOUNT_ID/123456789012/g' k8s/deployment.yaml | kubectl apply -f -
```

### 3. Deploy to Kubernetes (without IAM role)

```bash
kubectl apply -f k8s/deployment.yaml
```

### 3. Configure AWS Region and EKS Cluster

Update the ConfigMap in `k8s/deployment.yaml` with your specific values:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: rafay-relay-sop-config
  namespace: rafay-core
data:
  aws-region: "your-aws-region"
  eks-cluster: "your-eks-cluster-name"
```

### 4. Verify Deployment

```bash
kubectl get pods -n rafay-core -l app=rafay-relay-sop
kubectl get svc -n rafay-core rafay-relay-sop
```

## Configuration

The service can be configured via environment variables or ConfigMap:

| Variable | Description | Default |
|----------|-------------|---------|
| `AWS_REGION` | AWS region for EKS cluster | `us-west-2` |
| `EKS_CLUSTER` | EKS cluster name | `rafay-cluster` |

## Multi-Architecture Support

The application supports multiple CPU architectures:

- **AMD64** (x86_64) - For Intel/AMD processors
- **ARM64** (aarch64) - For ARM processors (Apple Silicon, AWS Graviton, etc.)

Multi-arch builds are supported using Docker Buildx, allowing the same image to run on different CPU architectures without modification.

## Architecture

- **Main Service** (`main.go`): HTTP server and request handling
- **SOP Service** (`sop_service.go`): Core SOP execution logic
- **AWS Operations** (`aws_operations.go`): EKS and Auto Scaling Group operations
- **K8S Operations** (`k8s_operations.go`): Kubernetes pod and node management

### Key Features

- **Concurrent Execution**: SOP runs in background, status can be checked via API
- **Comprehensive Logging**: Each step is logged with timestamps
- **Error Handling**: Detailed error reporting and rollback capabilities
- **AWS Integration**: Direct integration with EKS and Auto Scaling Groups
- **Kubernetes Native**: Uses Kubernetes client-go for cluster operations

## Security

- Runs as non-root user in container
- Uses Kubernetes ServiceAccount with minimal required permissions
- **IAM Roles for Service Accounts (IRSA)** for secure AWS access
- No need to store AWS credentials in the container
- All sensitive operations are logged for audit purposes

### IAM Permissions

The service requires the following AWS permissions:

- **EKS**: List and describe clusters, nodegroups
- **Auto Scaling**: Describe and update Auto Scaling Groups, manage instance protection
- **EC2**: Describe instances and their attributes
- **IAM**: Get and pass roles for EKS nodegroups

These permissions are defined in `k8s/iam-policy.json` and are automatically configured when using the setup script.

## Monitoring

The service provides health checks and can be monitored via:

- Kubernetes liveness/readiness probes
- `/health` endpoint for external monitoring
- Structured JSON logging
- Status endpoint for execution monitoring

## Troubleshooting

### Common Issues

1. **Permission Denied**: Ensure ServiceAccount has required RBAC permissions
2. **AWS Authentication**: Verify IAM roles or credentials are properly configured
3. **Node Group Not Found**: Check that nodegroup exists with correct label
4. **Deployment Scaling Failed**: Verify deployment exists in rafay-core namespace

### Debug Commands

```bash
# Check service logs
kubectl logs -n rafay-core -l app=rafay-relay-sop

# Check service status
kubectl get pods -n rafay-core -l app=rafay-relay-sop

# Test service connectivity
kubectl port-forward -n rafay-core svc/rafay-relay-sop 8080:8080
curl http://localhost:8080/health
```

## Development

### Local Development

```bash
# Install dependencies
go mod tidy

# Run locally
go run *.go

# Build binary
go build -o rafay-relay-sop .
```

### Testing

```bash
# Run tests
go test ./...

# Test with mock data
go test -v ./...
```

## License

This project is licensed under the MIT License.
