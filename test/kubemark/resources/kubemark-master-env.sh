# Generic variables.
INSTANCE_PREFIX="aleksandram"
SERVICE_CLUSTER_IP_RANGE="10.0.0.0/16"

# Etcd related variables.
ETCD_IMAGE="3.0.17"
ETCD_VERSION=""

# Controller-manager related variables.
CONTROLLER_MANAGER_TEST_ARGS="--v=2    --enable-garbage-collector=true"
ALLOCATE_NODE_CIDRS="true"
CLUSTER_IP_RANGE="10.224.0.0/11"
TERMINATED_POD_GC_THRESHOLD="100"

# Scheduler related variables.
SCHEDULER_TEST_ARGS="--v=2  "

# Apiserver related variables.
APISERVER_TEST_ARGS="--runtime-config=extensions/v1beta1 --v=2   --delete-collection-workers=16 --enable-garbage-collector=true"
STORAGE_BACKEND=""
NUM_NODES="1"
CUSTOM_ADMISSION_PLUGINS="Initializers,NamespaceLifecycle,LimitRanger,ServiceAccount,PodPreset,DefaultTolerationSeconds,NodeRestriction,ResourceQuota"
