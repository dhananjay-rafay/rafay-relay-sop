#!/bin/bash

# Setup script for IAM Roles for Service Accounts (IRSA)
set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Function to print colored output
print_status() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Function to show usage
show_usage() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  --cluster-name NAME    EKS cluster name (required)"
    echo "  --region REGION        AWS region (required)"
    echo "  --account-id ID        AWS account ID (required)"
    echo "  --namespace NAMESPACE  Kubernetes namespace (default: rafay-core)"
    echo "  --service-account SA   Service account name (default: rafay-relay-sop-sa)"
    echo "  --role-name ROLE       IAM role name (default: rafay-relay-sop-role)"
    echo "  -h, --help             Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0 --cluster-name my-cluster --region us-west-2 --account-id 123456789012"
}

# Default values
CLUSTER_NAME=""
REGION=""
ACCOUNT_ID=""
NAMESPACE="rafay-core"
SERVICE_ACCOUNT="rafay-relay-sop-sa"
ROLE_NAME="rafay-relay-sop-role"

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --cluster-name)
            CLUSTER_NAME="$2"
            shift 2
            ;;
        --region)
            REGION="$2"
            shift 2
            ;;
        --account-id)
            ACCOUNT_ID="$2"
            shift 2
            ;;
        --namespace)
            NAMESPACE="$2"
            shift 2
            ;;
        --service-account)
            SERVICE_ACCOUNT="$2"
            shift 2
            ;;
        --role-name)
            ROLE_NAME="$2"
            shift 2
            ;;
        -h|--help)
            show_usage
            exit 0
            ;;
        *)
            print_error "Unknown option: $1"
            show_usage
            exit 1
            ;;
    esac
done

# Validate required parameters
if [ -z "$CLUSTER_NAME" ] || [ -z "$REGION" ] || [ -z "$ACCOUNT_ID" ]; then
    print_error "Missing required parameters. Use --help for usage information."
    exit 1
fi

print_status "Setting up IAM Roles for Service Accounts (IRSA)"
print_status "Cluster: $CLUSTER_NAME"
print_status "Region: $REGION"
print_status "Account ID: $ACCOUNT_ID"
print_status "Namespace: $NAMESPACE"
print_status "Service Account: $SERVICE_ACCOUNT"
print_status "IAM Role: $ROLE_NAME"

# Check if eksctl is installed
if ! command -v eksctl &> /dev/null; then
    print_error "eksctl is not installed. Please install eksctl first."
    exit 1
fi

# Check if kubectl is installed
if ! command -v kubectl &> /dev/null; then
    print_error "kubectl is not installed. Please install kubectl first."
    exit 1
fi

# Check if aws CLI is installed
if ! command -v aws &> /dev/null; then
    print_error "AWS CLI is not installed. Please install AWS CLI first."
    exit 1
fi

# Step 1: Enable OIDC provider for the cluster
print_status "Step 1: Enabling OIDC provider for the cluster..."
eksctl utils associate-iam-oidc-provider --cluster $CLUSTER_NAME --region $REGION --approve

# Step 2: Get the OIDC provider ID
print_status "Step 2: Getting OIDC provider ID..."
OIDC_PROVIDER=$(aws eks describe-cluster --name $CLUSTER_NAME --region $REGION --query "cluster.identity.oidc.issuer" --output text | cut -d '/' -f 5)
print_status "OIDC Provider ID: $OIDC_PROVIDER"

# Step 3: Create IAM policy
print_status "Step 3: Creating IAM policy..."
POLICY_ARN=$(aws iam create-policy \
    --policy-name ${ROLE_NAME}-policy \
    --policy-document file://k8s/iam-policy.json \
    --query 'Policy.Arn' \
    --output text 2>/dev/null || \
    aws iam get-policy --policy-arn arn:aws:iam::${ACCOUNT_ID}:policy/${ROLE_NAME}-policy --query 'Policy.Arn' --output text)
print_status "Policy ARN: $POLICY_ARN"

# Step 4: Create IAM role with trust policy
print_status "Step 4: Creating IAM role..."
# Update trust policy with actual values
sed "s/ACCOUNT_ID/$ACCOUNT_ID/g; s/REGION/$REGION/g; s/EXAMPLED539D4633E53DE1B71EXAMPLE/$OIDC_PROVIDER/g; s/rafay-core/$NAMESPACE/g; s/rafay-relay-sop-sa/$SERVICE_ACCOUNT/g" \
    k8s/iam-trust-policy.json > /tmp/trust-policy.json

aws iam create-role \
    --role-name $ROLE_NAME \
    --assume-role-policy-document file:///tmp/trust-policy.json \
    --query 'Role.Arn' \
    --output text 2>/dev/null || print_warning "Role already exists"

# Step 5: Attach policy to role
print_status "Step 5: Attaching policy to role..."
aws iam attach-role-policy \
    --role-name $ROLE_NAME \
    --policy-arn $POLICY_ARN

# Step 6: Create namespace if it doesn't exist
print_status "Step 6: Creating namespace if it doesn't exist..."
kubectl create namespace $NAMESPACE --dry-run=client -o yaml | kubectl apply -f -

# Step 7: Update deployment with correct role ARN
print_status "Step 7: Updating deployment with IAM role ARN..."
ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}"
sed "s/ACCOUNT_ID/$ACCOUNT_ID/g; s/rafay-relay-sop-role/$ROLE_NAME/g" \
    k8s/deployment.yaml > /tmp/deployment-with-role.yaml

print_status "Setup completed successfully!"
print_status ""
print_status "Next steps:"
print_status "1. Deploy the application:"
print_status "   kubectl apply -f /tmp/deployment-with-role.yaml"
print_status ""
print_status "2. Verify the service account has the correct annotation:"
print_status "   kubectl get serviceaccount $SERVICE_ACCOUNT -n $NAMESPACE -o yaml"
print_status ""
print_status "3. Test the deployment:"
print_status "   kubectl get pods -n $NAMESPACE -l app=rafay-relay-sop"
print_status ""
print_status "IAM Role ARN: $ROLE_ARN"
print_status "OIDC Provider: $OIDC_PROVIDER"
