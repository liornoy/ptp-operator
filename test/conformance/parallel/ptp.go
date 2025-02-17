//go:build !unittests
// +build !unittests

package test

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	testclient "github.com/openshift/ptp-operator/test/pkg/client"
	"github.com/openshift/ptp-operator/test/pkg/event"
	"github.com/openshift/ptp-operator/test/pkg/execute"
	"github.com/openshift/ptp-operator/test/pkg/ptphelper"
	"github.com/openshift/ptp-operator/test/pkg/ptptesthelper"
	"github.com/openshift/ptp-operator/test/pkg/testconfig"
	v1core "k8s.io/api/core/v1"

	. "github.com/onsi/gomega"
	ptptestconfig "github.com/openshift/ptp-operator/test/conformance/config"
	"github.com/openshift/ptp-operator/test/pkg/metrics"
	exports "github.com/redhat-cne/ptp-listener-exports"
	ptpEvent "github.com/redhat-cne/sdk-go/pkg/event/ptp"
	"github.com/sirupsen/logrus"
)

const (
	clockSyncStateLocalForwardPort = 8901
	clockSyncStateLocalHttpPort    = 8902
)

var DesiredMode = testconfig.GetDesiredConfig(true).PtpModeDesired

// this full config is one per thread
var fullConfig = testconfig.TestConfig{}
var _ = Describe("["+strings.ToLower(DesiredMode.String())+"-parallel]", func() {

	var testParameters *ptptestconfig.PtpTestConfig

	execute.BeforeAll(func() {
		var err error
		testParameters, err = ptptestconfig.GetPtpTestConfig()
		Expect(err).To(BeNil(), "Failed to get Test Config")

		testclient.Client = testclient.New("")
		Expect(testclient.Client).NotTo(BeNil())

		// let ptp synchronize first
		time.Sleep(60 * time.Second)
	})

	Context("Soak testing", func() {
		BeforeEach(func() {
			if fullConfig.Status == testconfig.DiscoveryFailureStatus {
				Skip("Failed to find a valid ptp slave configuration")
			}
		})
		It("PTP CPU Utilization", func() {
			testPtpCpuUtilization(fullConfig, testParameters)
		})
	})

	Context("Event based tests", func() {
		BeforeEach(func() {

			if !ptphelper.PtpEventEnabled() {
				Skip("Skipping, PTP events not enabled")
			}
			logrus.Debugf("fullConfig=%s", fullConfig.String())
			if fullConfig.Status == testconfig.DiscoveryFailureStatus {
				Skip("Failed to find a valid ptp slave configuration")
			}
		})

		It("PTP Slave Clock Sync", func() {

			testPtpSlaveClockSync(fullConfig, testParameters) // Implementation of the test case

		})
		AfterEach(func() {
			// closing internal pubsub
			event.PubSub.Close()
		})
	})
})

// test case for continuous testing of clock synchronization of the clock under test
func testPtpSlaveClockSync(fullConfig testconfig.TestConfig, testParameters *ptptestconfig.PtpTestConfig) {
	event.InitPubSub()
	Expect(testclient.Client).NotTo(BeNil())
	logrus.Debugf("sync test fullConfig=%s", fullConfig.String())
	if fullConfig.Status == testconfig.DiscoveryFailureStatus {
		Fail("failed to find a valid ptp slave configuration")
	}

	if testParameters.SoakTestConfig.DisableSoakTest {
		Skip("skip the test as the entire suite is disabled")
	}

	soakTestConfig := testParameters.SoakTestConfig
	slaveClockSyncTestSpec := testParameters.SoakTestConfig.SlaveClockSyncConfig.TestSpec

	if !slaveClockSyncTestSpec.Enable {
		Skip("skip the test - the test is disabled")
	}

	logrus.Info("Test description ", soakTestConfig.SlaveClockSyncConfig.Description)

	// populate failure threshold
	failureThreshold := slaveClockSyncTestSpec.FailureThreshold
	if failureThreshold == 0 {
		failureThreshold = soakTestConfig.FailureThreshold
	}
	if failureThreshold == 0 {
		failureThreshold = 1
	}
	logrus.Info("Failure threshold = ", failureThreshold)
	// Actual implementation
	testSyncState(soakTestConfig, fullConfig)
}

// This test will run for configured minutes or until failure_threshold reached,
// whatever comes first. A failure_threshold is reached each time the cpu usage
// of the sum of the cpu usage of all the ptp pods (daemonset & operator) deployed
// in the same node is higher than the expected one. The cpu usage check for each
// node is once per minute.
func testPtpCpuUtilization(fullConfig testconfig.TestConfig, testParameters *ptptestconfig.PtpTestConfig) {
	const (
		minimumFailureThreshold  = 1
		cpuUsageCheckingInterval = 1 * time.Minute
	)

	logrus.Infof("CPU Utilization TC Config: %+v", testParameters.SoakTestConfig.CpuUtilization)

	if testParameters.SoakTestConfig.DisableSoakTest {
		Skip("skip the test as the entire suite is disabled")
	}

	params := testParameters.SoakTestConfig.CpuUtilization
	if !params.CpuTestSpec.Enable {
		Skip("skip the test - the test is disabled")
		return
	}

	// Set failureThresold limit number.
	failureThreshold := minimumFailureThreshold
	if params.CpuTestSpec.FailureThreshold > minimumFailureThreshold {
		failureThreshold = params.CpuTestSpec.FailureThreshold
	}

	prometheusPod, err := metrics.GetPrometheusPod()
	Expect(err).To(BeNil(), "failed to get prometheus pod")

	ptpPodsPerNode, err := ptptesthelper.GetPtpPodsPerNode()
	Expect(err).To(BeNil(), "failed to get ptp pods per node")

	prometheusRateTimeWindow, err := params.PromRateTimeWindow()
	Expect(err).To(BeNil(), "Invalid prometheus time window for prometheus' rate function.")

	cadvisorScrapeInterval, err := metrics.GetCadvisorScrapeInterval()
	Expect(err).To(BeNil(), "failed to get cadvisor's prometheus scrape interval")

	logrus.Infof("Configured rate timeWindow: %s, cadvisor scrape interval: %d secs.", prometheusRateTimeWindow, cadvisorScrapeInterval)
	// Make sure the configured time interval for prometheus's rate() func is at least twice
	// the current scrape interval for the kubelet's cadvisor endpoint. Otherwise, rate() will
	// never get the minimum samples number (2) to work.
	Expect(int(prometheusRateTimeWindow.Seconds())).To(BeNumerically(">=", 2*cadvisorScrapeInterval),
		fmt.Sprintf("configured time window (%s) is lower than twice the cadvisor scraping interval (%d secs)",
			prometheusRateTimeWindow, cadvisorScrapeInterval))

	// Warmup: waiting until prometheus can scrape a couple of cpu samples from ptp pods.
	warmupTime := time.Duration(2*cadvisorScrapeInterval) * time.Second
	By(fmt.Sprintf("Waiting %s so prometheus can get at least 2 metric samples from the ptp pods.", warmupTime))

	time.Sleep(warmupTime)

	// Create timer channel for test case timeout.
	testCaseDuration := time.Duration(params.CpuTestSpec.Duration) * time.Minute
	tcEndChan := time.After(testCaseDuration)

	// Create ticker for cpu usage checker function.
	cpuUsageCheckTicker := time.NewTicker(cpuUsageCheckingInterval)

	logrus.Infof("Running test for %s (failure threshold: %d)", testCaseDuration.String(), failureThreshold)

	failureCounter := 0
	for {
		select {
		case <-tcEndChan:
			// TC ended: report & return.
			logrus.Infof("CPU utilization threshold reached %d times.", failureCounter)
			return
		case <-cpuUsageCheckTicker.C:
			logrus.Infof("Retrieving cpu usage of the ptp pods.")

			thresholdReached, err := isCpuUsageThresholdReachedInPtpPods(prometheusPod, ptpPodsPerNode, &params)
			logrus.Infof("Cpu usage threshold reached: %v", thresholdReached)
			Expect(err).To(BeNil(), "failed to get cpu usage")

			if thresholdReached {
				failureCounter++
				Expect(failureCounter).To(BeNumerically("<", failureThreshold),
					fmt.Sprintf("Failure threshold (%d) reached", failureThreshold))
			}
		}
	}
}

// isCpuUsageThresholdReachedInPtpPods is a helper that checks whether the cpu usage of
// each node, pod and or container is below preconfigured (via yaml) threshold/s.
func isCpuUsageThresholdReachedInPtpPods(prometheusPod *v1core.Pod, ptpPodsPerNode map[string][]*v1core.Pod, cpuTestConfig *ptptestconfig.CpuUtilization) (bool, error) {
	thresholdReached := false

	// No need to check error for the rateTimeWindow: it was already checked.
	rateTimeWindow, _ := cpuTestConfig.PromRateTimeWindow()

	checkNodeTotalCpuUsage, nodeCpuUsageThreshold := cpuTestConfig.ShouldCheckNodeTotalCpuUsage()

	for nodeName, ptpPods := range ptpPodsPerNode {
		nodeTotalCpuUsage := float64(0)

		for i := range ptpPods {
			pod := ptpPods[i]

			cpuUsage, err := ptptesthelper.GetPodTotalCpuUsage(pod.Name, pod.Namespace, rateTimeWindow, prometheusPod)
			if err != nil {
				return false, fmt.Errorf("failed to get total cpu usage for ptp pods on node %s: %w", nodeName, err)
			}

			logrus.Infof("Node %s: pod: %s (ns:%s) cpu usage: %.5f", nodeName, pod.Name, pod.Namespace, cpuUsage)

			// Accumulate ptp pod cpu usage for this node.
			nodeTotalCpuUsage += cpuUsage

			// Should we check the total cpu usage for this pod?
			checkCpuUsage, cpuUsageThreshold := cpuTestConfig.ShouldCheckPodCpuUsage(pod.Name)
			if checkCpuUsage {
				logrus.Debugf("Checking cpu usage of pod %s. Cpu Usage: %.5f - Threshold: %.5f", pod.Name, cpuUsage, cpuUsageThreshold)
				if cpuUsage > cpuUsageThreshold {
					logrus.Warnf("Node %s: ptp pod %s cpu usage %.5f is higher than threshold %v", nodeName, pod.Name, cpuUsage, cpuUsageThreshold)
					thresholdReached = true
				}
			}

			for i := range pod.Spec.Containers {
				container := &pod.Spec.Containers[i]
				cpuUsage, err := ptptesthelper.GetContainerCpuUsage(pod.Name, container.Name, pod.Namespace, rateTimeWindow, prometheusPod)
				if err != nil {
					return false, fmt.Errorf("failed to get total cpu usage for ptp pods on node %s: %w", nodeName, err)
				}

				logrus.Infof("Node %s: pod: %s, container: %s (ns:%s) cpu usage: %.5f", nodeName, pod.Name, container.Name, pod.Namespace, cpuUsage)

				// Should we check the total cpu usage for this container?
				checkCpuUsage, cpuUsageThreshold := cpuTestConfig.ShouldCheckContainerCpuUsage(pod.Name, container.Name)
				if !checkCpuUsage {
					continue
				}

				logrus.Debugf("Checking cpu usage of container %s (pod %s). Cpu Usage: %.5f - Threshold: %.5f", container.Name, pod.Name, cpuUsage, cpuUsageThreshold)
				if cpuUsage > cpuUsageThreshold {
					logrus.Warnf("Node %s: ptp container %s (pod %s) cpu usage %.5f is higher than threshold %v",
						nodeName, container.Name, pod.Name, cpuUsage, cpuUsageThreshold)
					thresholdReached = true
				}
			}
		}

		logrus.Infof("Node %s: total cpu usage: %.5f", nodeName, nodeTotalCpuUsage)
		if checkNodeTotalCpuUsage {
			logrus.Debugf("Checking cpu usage of node %s, cpu:%v, threshold:%v", nodeName, nodeTotalCpuUsage, nodeCpuUsageThreshold)
			if nodeTotalCpuUsage > nodeCpuUsageThreshold {
				logrus.Warnf("Node %s: ptp pods cpu usage %.5f is higher than threshold %v",
					nodeName, nodeTotalCpuUsage, nodeCpuUsageThreshold)
				thresholdReached = true
			}
		}
	}

	return thresholdReached, nil
}

// Implementation for continuous testing of clock synchronization of the clock under test
func testSyncState(soakTestConfig ptptestconfig.SoakTestConfig, fullConfig testconfig.TestConfig) {
	// buffer to hold events until they can be processed. Buffering is needed to avoid dropping POST messages at the HTML server
	// During testing maximum buffer length could reach 20. Increase it as needed if the length reaches the capacity (see logs)
	const incomingEventsBuffer = 100
	slaveClockSyncTestSpec := soakTestConfig.SlaveClockSyncConfig.TestSpec
	logrus.Infof("%+v", slaveClockSyncTestSpec)
	syncEvents := ""
	// Create timer channel for test case timeout.
	testCaseDuration := time.Duration(slaveClockSyncTestSpec.Duration) * time.Minute
	tcEndChan := time.After(testCaseDuration)
	// registers channel to receive OsClockSyncStateChange events using the ptp-listener-lib
	tcEventChan, subscriberID := event.PubSub.Subscribe(string(ptpEvent.OsClockSyncStateChange), incomingEventsBuffer)
	// unsubscribe event type when finished
	defer event.PubSub.Unsubscribe(string(ptpEvent.OsClockSyncStateChange), subscriberID)
	// creates and push an initial event indicating the initial state of the clock
	// otherwise no events would be received as long as the clock is not changing states
	err := event.PushInitialEvent(string(ptpEvent.OsClockSyncStateChange), 20*time.Second)
	if err != nil {
		Fail(fmt.Sprintf("could not push initial event, err=%s", err))
	}
	term, err := event.MonitorPodLogsRegex()
	if err != nil {
		Fail(fmt.Sprintf("could not start listening to events, err=%s", err))
	}
	defer func() { term <- true }()
	// counts number of times the clock state looses LOCKED state
	failureCounter := 0
	wasLocked := false
	for {
		select {
		case <-tcEndChan:
			// The os clock never reach LOCKED status and the test has timed out
			if !wasLocked {
				Fail("OS Clock was never LOCKED and test timed out")
			}

			// add the events to the junit report
			AddReportEntry(fmt.Sprintf("%v", syncEvents))

			// Test case ended, pushing metrics
			logrus.Infof("Clock Sync failed %d times.", failureCounter)
			logrus.Infof("Collected sync events during soak test period= %s", syncEvents)
			ptphelper.SaveStoreEventsToFile(syncEvents, soakTestConfig.EventOutputFile)

			// if the number of loss of lock events exceed test threshold, fail the test
			Expect(failureCounter).To(BeNumerically("<", slaveClockSyncTestSpec.FailureThreshold),
				fmt.Sprintf("Failure threshold (%d) reached", slaveClockSyncTestSpec.FailureThreshold))
			return
		case singleEvent := <-tcEventChan:
			// New OsClockSyncStateChange event received
			logrus.Debugf("Received a new OsClockSyncStateChange event")
			logrus.Debugf("got %v\n", singleEvent)
			// get event values
			values, _ := singleEvent[exports.EventValues].(exports.StoredEventValues)
			state, _ := values["notification"].(string)
			clockOffset, _ := values["metric"].(float64)
			// create a pseudo value mapping a state to an integer (for visualization)
			eventString := fmt.Sprintf("%s,%s,%f,%s,%d\n", singleEvent[exports.EventTimeStamp], ptpEvent.OsClockSyncStateChange, clockOffset, state, exports.ToLockStateValue[state])
			// start counting loss of LOCK only after the clock was locked once
			logrus.Debugf("clockOffset=%f", clockOffset)
			if state != "LOCKED" && wasLocked {
				failureCounter++
			}

			// Wait for the clock to be locked at least once before stating to count failures
			if !wasLocked && state == "LOCKED" {
				wasLocked = true
				logrus.Info("Clock is locked, starting to monitor status now")
			}

			// wait before the clock was locked once before starting to record metrics
			if wasLocked {
				syncEvents += eventString
			}
		}
	}
}
