/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package autoscaling

import (
	"flag"
	"fmt"
	"math"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	testutils "k8s.io/kubernetes/test/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"github.com/golang/glog"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	memoryReservationTimeout = 5 * time.Minute
	largeResizeTimeout       = 8 * time.Minute
	largeScaleUpTimeout      = 20 * time.Minute
	largeScaleDownTimeout    = 20 * time.Minute
	minute                   = 1 * time.Minute
)

var (
	maxNodesFlag    = flag.Int("max-nodes", 10, "maximum number of nodes in test node pools")
	podsPerNodeFlag = flag.Int("pods-per-node", 30, "number of pods per node")
	controllersFlag = flag.Int("controllers", 1, "number of controllers to use")
)

type scaleUpTestConfig struct {
	extraPods     []*testutils.RCConfig
	expectedNodes int
}

var _ = framework.KubeDescribe("Cluster size autoscaler scalability [Slow]", func() {
	f := framework.NewDefaultFramework("autoscaling")
	var c clientset.Interface
	// Test suite parameters.
	var maxNodes int
	var podsPerNode int
	var controllers int
	// Implicit parameters from existing cluster.
	var memCapacityMb int
	var originalSizes map[string]int
	var originalNodeCount int
	// To be refreshed before every tests - starting number of nodes.
	var nodeCount int

	// We don't have access to gingko's BeforeSuite, but we want to have sth like this.
	// Therefore, in the first BeforeEach executed, we'll run this.
	beforeSuite := func() {
		// First time this is running.
		maxNodes = *maxNodesFlag
		podsPerNode = *podsPerNodeFlag
		controllers = *controllersFlag

		originalSizes = make(map[string]int)
		sum := 0
		framework.Logf("Node groups: %s", framework.TestContext.CloudConfig.NodeInstanceGroup)
		for _, mig := range strings.Split(framework.TestContext.CloudConfig.NodeInstanceGroup, ",") {
			size, err := framework.GroupSize(mig)
			framework.ExpectNoError(err)
			By(fmt.Sprintf("Initial size of %s: %d", mig, size))
			originalSizes[mig] = size
			sum += size
		}

		originalNodeCount = sum
		framework.ExpectNoError(framework.WaitForReadyNodes(c, sum, scaleUpTimeout))

		nodes := framework.GetReadyNodesIncludingTaintedOrDie(f.ClientSet)
		nodeCount = len(nodes.Items)
		Expect(nodeCount).NotTo(BeZero())
		Expect(nodeCount).Should(Equal(sum))

		template, err := getSampleNode(nodes.Items)
		if err != nil {
			framework.ExpectNoError(err)
		}
		mem := template.Status.Allocatable[v1.ResourceMemory]
		memCapacityMb = int((&mem).Value() / 1024 / 1024)
	}

	// Shared before each (to be executed before every single test)
	BeforeEach(func() {
		// Skip if not on supported cloud provider.
		framework.SkipUnlessProviderIs("gce", "gke", "kubemark")

		// Check if Cluster Autoscaler is enabled by trying to get its ConfigMap.
		_, err := f.ClientSet.CoreV1().ConfigMaps("kube-system").Get("cluster-autoscaler-status", metav1.GetOptions{})
		if err != nil {
			framework.Failf("test expects Cluster Autoscaler to be enabled")
		}

		c = f.ClientSet

		// "BeforeSuite" part to get some initial settings.
		if originalSizes == nil {
			beforeSuite()
		}
	})

	// Guaranteed to be executed just before every It block.
	// If more JustBeforeEach were added within Context blocks,
	// they'd run after this one... but let's not do that, to avoid confusion.
	JustBeforeEach(func() {
		// Set node count.
		nodes, err := getTestNodes(f.ClientSet)
		framework.ExpectNoError(err)
		nodeCount = len(nodes)
		framework.Logf("Node count: %v", nodeCount)
	})

	createRCConfigs := func(prefix string, additionalNodes, podsPerNode int, reservation float64, ports map[string]int) []*testutils.RCConfig {
		return createRCConfigsDetailed(f, prefix, additionalNodes, podsPerNode, reservation, ports, controllers, memCapacityMb)
	}

	createRCConfigsDefault := func(prefix string, additionalNodes int) []*testutils.RCConfig {
		// Configure pending pods for expected scale up.
		return createRCConfigs(prefix, additionalNodes, podsPerNode, 0.82, nil)
	}

	scalabilityScenarios := func() {
		It("should scale up to max [Feature:ClusterAutoscalerScalabilityScaleUp]", func() {
			expectedNodes := maxNodes
			extraNodes := maxNodes - nodeCount
			framework.Logf("Want %d nodes, will add pods to fill %d extra nodes", expectedNodes, extraNodes)
			rcConfigs := createRCConfigsDefault("extra-pod", extraNodes)
			config := createScaleUpTestConfig(nodeCount, 0, rcConfigs, expectedNodes)

			// Run test.
			// At large density, we may have incorrectly estimated the number of pods per node.
			// Scale up test won't have a problem with larger number, but it might with a smaller one.
			tolerance := int(0.1 * float64(extraNodes))
			if tolerance < 5 {
				tolerance = 5
			}
			testCleanup := simpleScaleUpTestWithTolerance(f, config, tolerance, 0)
			defer testCleanup()
		})

		It("should scale down empty nodes [Feature:ClusterAutoscalerScalabilityScaleDownEmpty]", func() {
			// Resize cluster to maxNodes to create empty nodes.
			extraPools := len(originalSizes) - 2
			maxSize := 1 + maxNodes/extraPools
			newSizes := newExtraPoolSizes(maxSize, originalSizes)
			start := time.Now()
			setMigSizes(newSizes)
			framework.ExpectNoError(waitForTestNodesCountFunc(f.ClientSet,
				func(size int) bool {
					return size >= maxSize*extraPools
				}, largeResizeTimeout))
			timeTrack(start, fmt.Sprintf("Resize to %d took", maxSize*extraPools))

			startScaleDown := time.Now()
			// Check if empty nodes are scaled down.
			tolerance := int(0.1 * float64(maxSize*extraPools-nodeCount))
			if tolerance < 5 {
				tolerance = 5
			}
			framework.ExpectNoError(waitForTestNodesCountFunc(f.ClientSet,
				func(size int) bool {
					return size <= nodeCount+tolerance
				}, largeScaleDownTimeout))
			timeTrack(startScaleDown, fmt.Sprintf("Scale down by %d took", maxSize*extraPools-nodeCount))
		})

		It("should scale down underutilized nodes [Feature:ClusterAutoscalerScalabilityScaleDownUnderutilized]", func() {
			if podsPerNode < 10 {
				// This test may not work well, reconsider?
			}

			extraNodes := maxNodes - nodeCount
			if extraNodes < 2 {
				framework.Skipf("Too few nodes left to run test, predicted extra nodes: %v", extraNodes)
			}
			additionalNodes70 := int(math.Floor(0.7 * float64(extraNodes)))
			additionalNodes30 := extraNodes - additionalNodes70

			// Reserve 70% / 30% space with host port deployments.
			hostPort30 := createRCConfigs("host-port-30", additionalNodes70, 1, 0.3, map[string]int{"port": 8888})
			hostPort70 := createRCConfigs("host-port-70", additionalNodes30, 1, 0.7, map[string]int{"port": 8888})
			hostPortRCConfigs := append(hostPort70, hostPort30...)
			expectedNodes := maxNodes
			hostPortConfig := createScaleUpTestConfig(nodeCount, 0, hostPortRCConfigs, expectedNodes)
			hostPortCleanup := simpleScaleUpTestWithTolerance(f, hostPortConfig, 3, 0)

			// Deploy the rest of pods.
			spaceToFill := additionalNodes30 + int(math.Floor(0.65*float64(additionalNodes70-additionalNodes30)))
			rcConfigs := createRCConfigsDefault("extra-pod", spaceToFill)
			config := createScaleUpTestConfig(nodeCount, 0, rcConfigs, expectedNodes)
			cleanup := simpleScaleUpTestWithTolerance(f, config, 0, 0)
			defer cleanup()

			start := time.Now()

			// Delete host port deployments so that some of the nodes are underutilized
			// and others have some space to move remaining pods there.
			hostPortCleanup()

			// Wait for scale down of additionalNodes30 or 30 nodes, whichever is smaller.
			//
			// Note: without soft taints, this is very unreliable.
			// Removing one of the nodes can reset counters for the others
			// if pods end up there. It's more likely to pass if we remove 30 out of
			// 300 nodes than if we're removing 3 out of 3.
			nodesToDelete := 30
			maxNodesToDelete := int(math.Floor(0.4 * float64(additionalNodes30)))
			if maxNodesToDelete < nodesToDelete {
				nodesToDelete = maxNodesToDelete
			}
			customScaleDownTimeout := time.Duration(nodesToDelete)*time.Minute + 20*time.Minute
			framework.ExpectNoError(waitForTestNodesCountFunc(f.ClientSet,
				func(size int) bool {
					return size <= maxNodes-nodesToDelete
				}, customScaleDownTimeout))
			timeTrack(start, fmt.Sprintf("Scale down with drain by %d took", maxNodes-nodesToDelete))
		})

	}

	var sharedCleanups []func() error
	sharedCleanup := func() {
		for _, cleanup := range sharedCleanups {
			cleanup()
		}
		sharedCleanups = nil
	}
	framework.AddCleanupAction(sharedCleanup)

	Context("when cluster is empty [Context:ClusterAutoscalerScalabilityEmptyCluster]", func() {
		BeforeEach(func() {
			// Clean up setup from other contexts.
			sharedCleanup()

			// Restore original sizes.
			By(fmt.Sprintf("Restoring initial size of the cluster"))
			setMigSizes(originalSizes)
			framework.ExpectNoError(framework.WaitForReadyNodes(c, originalNodeCount, scaleDownTimeout))

			// Make all nodes schedulable after resizes.
			makeSchedulable(c)
		})

		// Register scenarios to run
		scalabilityScenarios()
	})

	Context("when cluster is already large [Context:ClusterAutoscalerScalabilityLargeCluster]", func() {
		var fillerPodCleanup func() error

		BeforeEach(func() {
			// Ensure the cluster is already large, resize if it isn't.
			startingNodes := int(math.Floor(0.7 * float64(maxNodes)))
			extraPools := len(originalSizes) - 2

			// Resize cluster to startingNodes. This is approximate - if there are too few nodes, CA will add some for filler pods.
			newSize := startingNodes / extraPools
			newSizes := newExtraPoolSizes(newSize, originalSizes)
			setMigSizes(newSizes)
			framework.ExpectNoError(waitForTestNodesCountFunc(f.ClientSet,
				func(size int) bool {
					return size == newSize*extraPools
				}, largeResizeTimeout))

			if fillerPodCleanup == nil {
				// Clean up setup from other contexts first.
				sharedCleanup()

				// Run pods to make startingNodes nodes utilized. A couple new nodes may be added as a result.
				expectedNodes := newSize * extraPools
				rcConfigs := createRCConfigsDefault("filler-pod", expectedNodes)
				for _, rc := range rcConfigs {
					rc.Namespace = "kube-system"
				}
				config := createScaleUpTestConfig(nodeCount, 0, rcConfigs, expectedNodes)
				tolerance := int(0.1 * float64(expectedNodes))
				if tolerance < 3 {
					tolerance = 3
				}
				cleanup := simpleScaleUpTestWithTolerance(f, config, tolerance, 0)
				fillerPodCleanup = func() error {
					fillerPodCleanup = nil
					return cleanup()
				}
				sharedCleanups = append(sharedCleanups, fillerPodCleanup)
			}

			// Make all nodes schedulable after resizes.
			makeSchedulable(c)
		})

		// Register scenarios to run
		scalabilityScenarios()
	})

	Context("when empty cluster has a large number of unschedulable pods [Context:ClusterAutoscalerScalabilityUnschedulablePods]", func() {
		var unschedulablePodCleanup func() error

		BeforeEach(func() {
			// Create unschedulable pods.
			if unschedulablePodCleanup == nil {
				// Clean up setup from other contexts first.
				sharedCleanup()

				// Create pods to make traffic in scheduler's queue. No new nodes should be added as a result.
				rcConfigs := createRCConfigsDefault("unschedulable-pod", maxNodes)
				for _, rc := range rcConfigs {
					// Make not associated with framework's namespace to avoid cleanup after first test.
					rc.Namespace = "kube-system"
					// Make unschedulable.
					rc.NodeSelector["imaginary-label"] = "i"
					// And don't wait too long to fail - they won't run.
					rc.Timeout = time.Second
				}
				finalErrs := forEachRC(rcConfigs, func(rc *testutils.RCConfig) error { return framework.RunRC(*rc) })
				Expect(len(finalErrs)).Should(Equal(len(rcConfigs)))
				unschedulablePodCleanup = func() error {
					deleteFunc := func(rc *testutils.RCConfig) error {
						return framework.DeleteRCAndWaitForGC(c, rc.Namespace, rc.Name)
					}
					finalErrs := forEachRC(rcConfigs, deleteFunc)
					if len(finalErrs) > 0 {
						return fmt.Errorf("%v error deleting replication controllers: %v", len(finalErrs), finalErrs)
					}
					unschedulablePodCleanup = nil
					return nil
				}
				sharedCleanups = append(sharedCleanups, unschedulablePodCleanup)
			}

			// Restore original sizes.
			By(fmt.Sprintf("Restoring initial size of the cluster"))
			setMigSizes(originalSizes)
			framework.ExpectNoError(framework.WaitForReadyNodes(c, originalNodeCount, scaleDownTimeout))

			// Make all nodes schedulable before running tests.
			makeSchedulable(c)
		})

		// Register scenarios to run.
		scalabilityScenarios()
	})

})

func getSampleNode(nodes []v1.Node) (v1.Node, error) {
	for _, node := range nodes {
		if !strings.Contains(node.Labels[gkeNodepoolNameKey], "default-pool-large") {
			return node, nil
		} else {
			glog.Infof("Node %v is unsuitable, node-pool label: %v", node.Name, node.Labels[gkeNodepoolNameKey])
		}
	}
	return v1.Node{}, fmt.Errorf("No suitable sample node found, nodes: %v", nodes)
}

func getTestNodes(c clientset.Interface) ([]v1.Node, error) {
	nodes, err := c.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return []v1.Node{}, err
	}
	return filterNodes(nodes.Items, func(node v1.Node) bool {
		return strings.Contains(node.Labels["pool-type"], "test")
	}), nil
}

func filterNodes(nodes []v1.Node, filter func(v1.Node) bool) []v1.Node {
	filtered := make([]v1.Node, 0, len(nodes))
	for _, node := range nodes {
		if filter(node) {
			filtered = append(filtered, node)
		}
	}
	return filtered
}

func waitForTestNodesCountFunc(c clientset.Interface, sizeFunc func(int) bool, timeout time.Duration) error {
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(20 * time.Second) {
		nodes, err := getTestNodes(c)
		if err != nil {
			glog.Warningf("Failed to get test nodes: %v", err)
			continue
		}
		ready := filterNodes(nodes, func(node v1.Node) bool {
			return framework.IsNodeConditionSetAsExpected(&node, v1.NodeReady, true) && !node.Spec.Unschedulable
		})
		numReady := len(ready)
		if sizeFunc(numReady) {
			glog.Infof("Cluster has reached the desired size, node count: %v", numReady)
			return nil
		}
		glog.Infof("Waiting for cluster with func, current size %v, not ready nodes: %v", len(nodes), numReady)
	}
	return fmt.Errorf("timeout waiting %v for appropriate cluster size", timeout)
}

func forEachRC(rcConfigs []*testutils.RCConfig, doFunc func(*testutils.RCConfig) error) []error {
	errs := make(chan error, len(rcConfigs))
	for _, rc := range rcConfigs {
		go func(rc *testutils.RCConfig) {
			errs <- doFunc(rc)
		}(rc)
	}
	finalErrs := []error{}
	for range rcConfigs {
		if err := <-errs; err != nil {
			finalErrs = append(finalErrs, err)
		}
	}
	return finalErrs
}

func simpleScaleUpTestWithTolerance(f *framework.Framework, config *scaleUpTestConfig, tolerateMissingNodeCount int, tolerateMissingPodCount int) func() error {
	// Run RCs based on config.
	start := time.Now()
	runFunc := func(rc *testutils.RCConfig) error {
		By(fmt.Sprintf("Running RC %v from config", rc.Name))
		return framework.RunRC(*rc)
	}
	finalErrs := forEachRC(config.extraPods, runFunc)
	if len(finalErrs) > 0 {
		framework.ExpectNoError(fmt.Errorf("%v errors running RCs: %v", len(finalErrs), finalErrs))
	}

	// Tolerate some number of nodes not to be created.
	minExpectedNodeCount := config.expectedNodes - tolerateMissingNodeCount
	framework.ExpectNoError(waitForTestNodesCountFunc(f.ClientSet,
		func(size int) bool { return size >= minExpectedNodeCount }, scaleUpTimeout))
	glog.Infof("cluster is increased")
	// Tolerate some number of nodes not to be ready.
	if tolerateMissingPodCount > 0 {
		framework.ExpectNoError(waitForCaPodsReadyInNamespace(f, f.ClientSet, tolerateMissingPodCount))
	} else {
		framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, f.ClientSet))
	}

	timeTrack(start, fmt.Sprintf("Scale up to %v", config.expectedNodes))

	// Save client via closure in case framework is unusable by the time cleanup is called.
	// Sounds dangerous but kinda works?
	c := f.ClientSet
	return func() error {
		deleteFunc := func(rc *testutils.RCConfig) error {
			return framework.DeleteRCAndWaitForGC(c, rc.Namespace, rc.Name)
		}
		finalErrs := forEachRC(config.extraPods, deleteFunc)
		if len(finalErrs) > 0 {
			return fmt.Errorf("%v error deleting replication controllers: %v", len(finalErrs), finalErrs)
		}
		return nil
	}
}

func reserveMemoryRCConfig(f *framework.Framework, id string, replicas, megabytes int, timeout time.Duration) *testutils.RCConfig {
	return &testutils.RCConfig{
		Client:         f.ClientSet,
		InternalClient: f.InternalClientset,
		Name:           id,
		Namespace:      f.Namespace.Name,
		Timeout:        timeout,
		Image:          imageutils.GetPauseImageName(),
		Replicas:       replicas,
		MemRequest:     int64(1024 * 1024 * megabytes / replicas),
		NodeSelector:   map[string]string{"pool-type": "test"},
		Tolerations: []v1.Toleration{
			{
				Key:    "pool-type",
				Value:  "test",
				Effect: v1.TaintEffectNoSchedule,
			},
		},
	}
}

func createScaleUpTestConfig(nodes, pods int, extraPods []*testutils.RCConfig, expectedNodes int) *scaleUpTestConfig {
	return &scaleUpTestConfig{
		extraPods:     extraPods,
		expectedNodes: expectedNodes,
	}
}

func timeTrack(start time.Time, name string) {
	elapsed := time.Since(start)
	glog.Infof("%s took %s", name, elapsed)
}

func createRCConfigsDetailed(f *framework.Framework, prefix string, additionalNodes, podsPerNode int, reservation float64, ports map[string]int, controllers int, memCapacityMb int) []*testutils.RCConfig {
	perNodeReservation := int(float64(memCapacityMb) * reservation)
	replicas := additionalNodes * podsPerNode
	finalControllers := controllers
	if replicas < controllers {
		finalControllers = replicas
	}
	replicasPerController := int(replicas / finalControllers)
	overflow := replicas - (replicasPerController * finalControllers)
	additionalReservation := int(additionalNodes * perNodeReservation / finalControllers)
	rcConfigs := []*testutils.RCConfig{}
	for i := 0; i < finalControllers; i++ {
		r := replicasPerController
		if i < overflow {
			r++
		}
		rcConfig := reserveMemoryRCConfig(f, fmt.Sprintf("%s-%v", prefix, i), r, additionalReservation, largeScaleUpTimeout)
		if ports != nil {
			rcConfig.HostPorts = ports
		}
		rcConfigs = append(rcConfigs, rcConfig)
	}
	return rcConfigs
}

func makeSchedulable(c clientset.Interface) {
	nodes, err := c.CoreV1().Nodes().List(metav1.ListOptions{})
	framework.ExpectNoError(err)
	s := time.Now()
makeSchedulableLoop:
	for start := time.Now(); time.Since(start) < makeSchedulableTimeout; time.Sleep(makeSchedulableDelay) {
		for _, n := range nodes.Items {
			err = makeNodeSchedulable(c, &n, true)
			if err != nil {
				switch err.(type) {
				case CriticalAddonsOnlyError:
					continue makeSchedulableLoop
				default:
					nodes, err = c.CoreV1().Nodes().List(metav1.ListOptions{})
					if err != nil {
						framework.Logf("Error listing nodes: %v", err)
					}
					continue makeSchedulableLoop
				}
			}
		}
		break
	}
	glog.Infof("Made nodes schedulable again in %v", time.Since(s).String())
}

func newExtraPoolSizes(newSize int, originalSizes map[string]int) map[string]int {
	newSizes := map[string]int{}
	for group, oldSize := range originalSizes {
		if strings.Contains(group, "extra") {
			newSizes[group] = newSize
		} else {
			newSizes[group] = oldSize
		}
	}
	return newSizes
}
