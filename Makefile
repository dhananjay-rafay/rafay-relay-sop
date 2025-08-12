.PHONY: build test clean deploy docker-build docker-push help

# Variables
BINARY_NAME=rafay-relay-sop
DOCKER_IMAGE=rafay-relay-sop
DOCKER_TAG=latest
NAMESPACE=rafay-core

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod

# Build the application
build:
	$(GOBUILD) -o $(BINARY_NAME) .

# Run tests
test:
	$(GOTEST) -v ./...

# Clean build artifacts
clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)

# Install dependencies
deps:
	$(GOMOD) tidy
	$(GOMOD) download

# Run the application locally
run:
	$(GOCMD) run *.go

# Build Docker image
docker-build:
	docker build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

# Build multi-arch Docker image
docker-buildx:
	docker buildx build --platform linux/amd64,linux/arm64 -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

# Build and push multi-arch Docker image
docker-buildx-push:
	docker buildx build --platform linux/amd64,linux/arm64 -t $(DOCKER_IMAGE):$(DOCKER_TAG) --push .

# Push Docker image to registry
docker-push:
	docker push $(DOCKER_IMAGE):$(DOCKER_TAG)

# Setup IAM role for service account
setup-iam:
	@echo "Setting up IAM role for service account..."
	@echo "Usage: make setup-iam CLUSTER_NAME=<cluster> REGION=<region> ACCOUNT_ID=<account>"
	@echo "Example: make setup-iam CLUSTER_NAME=my-cluster REGION=us-west-2 ACCOUNT_ID=123456789012"
	@if [ -z "$(CLUSTER_NAME)" ] || [ -z "$(REGION)" ] || [ -z "$(ACCOUNT_ID)" ]; then \
		echo "Error: CLUSTER_NAME, REGION, and ACCOUNT_ID are required"; \
		exit 1; \
	fi
	chmod +x k8s/setup-iam.sh
	./k8s/setup-iam.sh --cluster-name $(CLUSTER_NAME) --region $(REGION) --account-id $(ACCOUNT_ID)

# Deploy to Kubernetes
deploy:
	kubectl apply -f k8s/deployment.yaml

# Deploy with IAM role (after running setup-iam)
deploy-with-iam:
	@if [ -z "$(ACCOUNT_ID)" ]; then \
		echo "Error: ACCOUNT_ID is required"; \
		echo "Usage: make deploy-with-iam ACCOUNT_ID=<account>"; \
		exit 1; \
	fi
	sed "s/ACCOUNT_ID/$(ACCOUNT_ID)/g" k8s/deployment.yaml | kubectl apply -f -

# Delete from Kubernetes
undeploy:
	kubectl delete -f k8s/deployment.yaml

# Check deployment status
status:
	kubectl get pods -n $(NAMESPACE) -l app=$(BINARY_NAME)
	kubectl get svc -n $(NAMESPACE) $(BINARY_NAME)

# View logs
logs:
	kubectl logs -n $(NAMESPACE) -l app=$(BINARY_NAME) -f

# Port forward for local testing
port-forward:
	kubectl port-forward -n $(NAMESPACE) svc/$(BINARY_NAME) 8080:8080

# Test the service
test-service:
	curl -XGET http://localhost:8080/health
	curl -XGET http://localhost:8080/status

# Run SOP
run-sop:
	curl -XGET http://localhost:8080/run

# Check SOP status
check-sop:
	curl -XGET http://localhost:8080/status

# Format code
fmt:
	$(GOCMD) fmt ./...

# Lint code
lint:
	golangci-lint run

# Security scan
security:
	gosec ./...

# Full build and test pipeline
pipeline: clean deps fmt lint test build

# Help
help:
	@echo "Available targets:"
	@echo "  build         - Build the application"
	@echo "  test          - Run tests"
	@echo "  clean         - Clean build artifacts"
	@echo "  deps          - Install dependencies"
	@echo "  run           - Run the application locally"
	@echo "  docker-build  - Build Docker image"
	@echo "  docker-buildx - Build multi-arch Docker image (AMD64 + ARM64)"
	@echo "  docker-buildx-push - Build and push multi-arch Docker image"
	@echo "  docker-push   - Push Docker image"
	@echo "  setup-iam     - Setup IAM role for service account"
	@echo "  deploy        - Deploy to Kubernetes"
	@echo "  deploy-with-iam - Deploy with IAM role (after setup-iam)"
	@echo "  undeploy      - Remove from Kubernetes"
	@echo "  status        - Check deployment status"
	@echo "  logs          - View application logs"
	@echo "  port-forward  - Port forward for local testing"
	@echo "  test-service  - Test the service endpoints"
	@echo "  run-sop       - Start SOP execution"
	@echo "  check-sop     - Check SOP execution status"
	@echo "  fmt           - Format code"
	@echo "  lint          - Lint code"
	@echo "  security      - Security scan"
	@echo "  pipeline      - Full build and test pipeline"
	@echo "  help          - Show this help message"
