#!/bin/bash

set -o errexit

PROJECT=aleksandram-gke-dev
ZONE=us-central1-c
ENV="test"
CLUSTER_NAME=ca-scale
CLUSTER_VERSION="1.11"
NODES=10
POOLS=2
PODS_PER_NODE=30
CONTROLLERS=3
MACHINE_TYPE=g1-small

# Script control
CREATE_CLUSTER=${CREATE_CLUSTER:-"true"}
DELETE_CLUSTER=${DELETE_CLUSTER:-"false"}
DEBUG=false

# Implicit values (will be overridden below)
CIDR_RANGE=/12
EXTRA_POOL_MAX=5
HEAPSTER_MACHINE_TYPE=n1-standard-4
GKE_ENDPOINT=https://test-container.sandbox.googleapis.com/
OLD_GKE_ENDPOINT=${CLOUDSDK_API_ENDPOINT_OVERRIDES_CONTAINER:-}

# Hard-coded defaults (modify here if required)
DEFAULT_POOL_MAX=20
HEAPSTER_POOL_MAX=12
LOG_NAME=ca-scale-run #TODO: make implicit - use timestamp + config

# Cluster setup
create-cluster() {
  # Create cluster
  echo "Creating cluster ${CLUSTER_NAME} in project ${PROJECT} and zone ${ZONE}..."
  gcloud container clusters create ${CLUSTER_NAME} \
    --cluster-version ${CLUSTER_VERSION} \
    --zone ${ZONE} \
    --project ${PROJECT} \
    --enable-autoscaling --num-nodes 1 --max-nodes ${DEFAULT_POOL_MAX} \
    --enable-ip-alias --cluster-ipv4-cidr=${CIDR_RANGE} \
    --disk-size 20gb \
    --machine-type ${MACHINE_TYPE}
  echo "Cluster created"

  # Create test node pools
  for i in $(seq 1 ${POOLS})
  do
    echo "Adding extra node pool ${i} out of ${POOLS}..."
    gcloud container node-pools create extra-pool${i} \
      --cluster ${CLUSTER_NAME} \
      --zone ${ZONE} \
      --project ${PROJECT} \
      --enable-autoscaling --num-nodes 0 --max-nodes ${EXTRA_POOL_MAX} \
      --node-labels pool-type=test --node-taints pool-type=test:NoSchedule \
      --disk-size 20gb \
      --machine-type ${MACHINE_TYPE}
  done

  # Create node pool for Heapster [*]
  echo "Adding Heapster node pool with machine type ${HEAPSTER_MACHINE_TYPE}..."
  gcloud container node-pools create default-pool-large \
    --cluster ${CLUSTER_NAME} \
    --zone ${ZONE} \
    --project ${PROJECT} \
    --machine-type ${HEAPSTER_MACHINE_TYPE} \
    --enable-autoscaling --num-nodes 0 --max-nodes ${HEAPSTER_POOL_MAX}

  echo "Cluster ready"
}

delete-cluster() {
  "Deleting test cluster..."
  gcloud container clusters delete ${CLUSTER_NAME} --zone ${ZONE} --project ${PROJECT}
}

# Calculate implicit variables
calculate-implicit() {
  if [ ${NODES} -gt 1000 ]
  then
    HEAPSTER_MACHINE_TYPE="n1-standard-32"
    CIDR_RANGE="/12"
  elif [ ${NODES} -gt 500 ]
  then
    HEAPSTER_MACHINE_TYPE="n1-standard-16"
    CIDR_RANGE="/13"
  else
    HEAPSTER_MACHINE_TYPE="n1-standard-4"
    CIDR_RANGE="/14"
  fi;

  if [ "${ENV}" == "test" ]
  then
    GKE_ENDPOINT="https://test-container.sandbox.googleapis.com/"
  fi;
  echo "Setting CLOUDSDK_API_ENDPOINT_OVERRIDES_CONTAINER=${GKE_ENDPOINT}"
  export CLOUDSDK_API_ENDPOINT_OVERRIDES_CONTAINER=${GKE_ENDPOINT}

  EXTRA_POOL_MAX=$(( ${NODES} / ${POOLS} + 1 ))
}

# Test
run-test() {
  # Run test
  echo "Running test, use the following command to track results:"
  echo "$ tail -f ${LOG_NAME}"
  go run hack/e2e.go -- \
    --cluster=${CLUSTER_NAME} \
    --gcp-zone=${ZONE} \
    --gcp-project=${PROJECT} \
    --test --check-version-skew=false -v 4 \
    --test_args="--ginkgo.focus=ClusterAutoscalerScalability1 --max-nodes ${NODES} --pods-per-node ${PODS_PER_NODE} --controllers ${CONTROLLERS}" \
    --provider=gke --deployment=gke --gcp-node-image=cos --gke-environment ${GKE_ENDPOINT} \
    --gcp-network=default > ${LOG_NAME}
  echo "Test completed"
}

# Cleanup shell.
cleanup() {
  echo "Setting CLOUDSDK_API_ENDPOINT_OVERRIDES_CONTAINER=${OLD_GKE_ENDPOINT}"
  export CLOUDSDK_API_ENDPOINT_OVERRIDES_CONTAINER=${OLD_GKE_ENDPOINT}
}

main() {
  if [ ${DEBUG} == "true" ]
  then
    set -o xtrace
  fi;

  calculate-implicit
  if [ ${CREATE_CLUSTER} == "true" ]
  then
    create-cluster
  fi;
  run-test
  if [ ${DELETE_CLUSTER} == "true" ]
  then
    delete-cluster
  fi;
  cleanup
}

main

