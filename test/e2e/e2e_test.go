/*
Copyright 2026.

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

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/lakekeeper/lakekeeper-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "lakekeeper-operator-system"

// serviceAccountName created for the project
const serviceAccountName = "lakekeeper-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "lakekeeper-operator-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "lakekeeper-operator-metrics-binding"

// getLakekeeperImage returns the Lakekeeper container image to use in tests.
// Can be overridden via LAKEKEEPER_IMAGE environment variable.
func getLakekeeperImage() string {
	if img := os.Getenv("LAKEKEEPER_IMAGE"); img != "" {
		return img
	}
	return "quay.io/lakekeeper/catalog:latest"
}

var _ = Describe("Manager", Serial, Ordered, func() {
	var controllerPodName string

	// Before running the tests, verify the controller is running.
	// Controller deployment is now handled in BeforeSuite so it stays running for all test suites.
	BeforeAll(func() {
		By("verifying controller-manager is running")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-n", namespace,
				"-o", "jsonpath={.items[0].status.phase}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Running"))
		}, "1m", "5s").Should(Succeed())
	})

	// After all Manager tests, clean up only test-specific resources.
	// DO NOT delete controller deployment or namespace - they're needed for Lakekeeper tests!
	// Controller cleanup happens in AfterSuite after all test suites complete.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics",
			"-n", namespace,
			"--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("cleaning up ClusterRoleBinding for metrics")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding",
			metricsRoleBindingName,
			"--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		// Controller deployment and namespace are NOT deleted here.
		// They stay running for Lakekeeper tests and are cleaned up in AfterSuite.
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=lakekeeper-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("controller-runtime.metrics\tServing metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccount": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			metricsOutput := getMetricsOutput()
			Expect(metricsOutput).To(ContainSubstring(
				"controller_runtime_reconcile_total",
			))
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})
})

var _ = Describe("Lakekeeper Deployment E2E", Serial, Ordered, func() {
	var (
		testNamespace  string
		lakekeeperName string
	)

	BeforeAll(func() {
		testNamespace = "lakekeeper-e2e-" + utils.RandString(8)
		lakekeeperName = "lakekeeper-test"
		setupNamespaceWithPostgres(testNamespace)
	})

	AfterAll(func() {
		By("Deleting test namespace: " + testNamespace)
		cmd := exec.Command("kubectl", "delete", "ns", testNamespace)
		_, _ = utils.Run(cmd)
	})

	Context("Lakekeeper instance lifecycle", func() {
		It("should deploy Lakekeeper with PostgreSQL and pass health checks", func() {
			By("Creating Lakekeeper CR")
			lakekeeperYAML := fmt.Sprintf(`
apiVersion: lakekeeper.k8s.lakekeeper.io/v1alpha1
kind: Lakekeeper
metadata:
  name: %s
  namespace: %s
spec:
  image: %s
  replicas: 1
%s
  authorization:
    backend: allowall
`, lakekeeperName, testNamespace, getLakekeeperImage(), defaultTestDBConfig(testNamespace))

			lakekeeperFile := filepath.Join("/tmp", fmt.Sprintf("lakekeeper-%s.yaml", testNamespace))
			err := os.WriteFile(lakekeeperFile, []byte(lakekeeperYAML), 0644)
			Expect(err).NotTo(HaveOccurred())

			cmd := exec.Command("kubectl", "apply", "-f", lakekeeperFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for Lakekeeper deployment to exist")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(lakekeeperName))
			}, "2m", "5s").Should(Succeed())

			By("Waiting for Lakekeeper deployment to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, "5m", "5s").Should(Succeed())

			By("Verifying Lakekeeper pod is running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", "app.kubernetes.io/name=lakekeeper",
					"-o", "jsonpath={.items[0].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}, "2m", "5s").Should(Succeed())
			var output string

			By("Verifying migration Job was created and completed")
			var migrationJobName string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "jobs",
					"-n", testNamespace,
					"-l", "app.kubernetes.io/component=migration",
					"-o", "jsonpath={.items[0].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				migrationJobName = output
			}, "2m", "5s").Should(Succeed())

			By("Verifying migration Job has hash-based naming")
			Expect(migrationJobName).To(MatchRegexp(fmt.Sprintf(`%s-migrate-[0-9a-f]{8}`, lakekeeperName)))

			By("Verifying migration Job completed successfully")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "job", migrationJobName,
					"-n", testNamespace,
					"-o", "jsonpath={.status.succeeded}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, "3m", "5s").Should(Succeed())

			By("Verifying migration Job has owner reference to Lakekeeper")
			cmd = exec.Command("kubectl", "get", "job", migrationJobName,
				"-n", testNamespace,
				"-o", "jsonpath={.metadata.ownerReferences[0].kind}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("Lakekeeper"))

			By("Verifying Lakekeeper status shows Migrated=True")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lakekeeper", lakekeeperName,
					"-n", testNamespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Migrated')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, "30s", "2s").Should(Succeed())

			By("Verifying Deployment has NO initContainers")
			cmd = exec.Command("kubectl", "get", "deployment", lakekeeperName,
				"-n", testNamespace,
				"-o", "jsonpath={.spec.template.spec.initContainers}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Or(BeEmpty(), Equal("[]")))

			By("Verifying main service exists")
			cmd = exec.Command("kubectl", "get", "service", lakekeeperName,
				"-n", testNamespace, "-o", "jsonpath={.spec.ports[0].port}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("8181"))

			By("Verifying metrics service exists")
			cmd = exec.Command("kubectl", "get", "service", lakekeeperName+"-metrics",
				"-n", testNamespace, "-o", "jsonpath={.spec.ports[0].port}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("9000"))

			By("Checking /health endpoint responds")
			Eventually(func(g Gomega) {
				body, err := httpGetService(testNamespace, lakekeeperName, 8181, "/health")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(body).NotTo(BeEmpty())
			}, "2m", "5s").Should(Succeed())

			By("Checking /metrics endpoint returns Prometheus metrics")
			Eventually(func(g Gomega) {
				body, err := httpGetService(testNamespace, lakekeeperName+"-metrics", 9000, "/metrics")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(body).To(ContainSubstring("# HELP"))
				g.Expect(body).To(ContainSubstring("# TYPE"))
			}, "1m", "5s").Should(Succeed())

			By("Verifying Lakekeeper status is Ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lakekeeper", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, "2m", "5s").Should(Succeed())

			By("Verifying replica counts in status")
			cmd = exec.Command("kubectl", "get", "lakekeeper", lakekeeperName,
				"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("1"))
		})

		It("should handle Lakekeeper instance deletion cleanly", func() {
			By("Deleting Lakekeeper CR")
			cmd := exec.Command("kubectl", "delete", "lakekeeper", lakekeeperName,
				"-n", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying deployment is deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName,
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}, "2m", "5s").Should(Succeed())

			By("Verifying services are deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", lakekeeperName,
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}, "1m", "5s").Should(Succeed())

			By("Verifying no orphaned pods remain")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", "app.kubernetes.io/name=lakekeeper", "-o", "jsonpath={.items}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("[]"))
			}, "2m", "5s").Should(Succeed())
		})
	})

	Context("Lakekeeper configuration updates", func() {
		BeforeEach(func() {
			By("Creating Lakekeeper instance for update tests")
			lakekeeperYAML := fmt.Sprintf(`
apiVersion: lakekeeper.k8s.lakekeeper.io/v1alpha1
kind: Lakekeeper
metadata:
  name: %s
  namespace: %s
spec:
  image: %s
  replicas: 1
%s
  authorization:
    backend: allowall
`, lakekeeperName, testNamespace, getLakekeeperImage(), defaultTestDBConfig(testNamespace))

			lakekeeperFile := filepath.Join("/tmp", fmt.Sprintf("lakekeeper-%s.yaml", testNamespace))
			err := os.WriteFile(lakekeeperFile, []byte(lakekeeperYAML), 0644)
			Expect(err).NotTo(HaveOccurred())

			cmd := exec.Command("kubectl", "apply", "-f", lakekeeperFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Wait for ready
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, "5m", "5s").Should(Succeed())
		})

		AfterEach(func() {
			By("Cleaning up Lakekeeper instance")
			cmd := exec.Command("kubectl", "delete", "lakekeeper", lakekeeperName,
				"-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should handle custom server ports", func() {
			By("Updating Lakekeeper with custom ports")
			cmd := exec.Command("kubectl", "patch", "lakekeeper", lakekeeperName,
				"-n", testNamespace, "--type=merge",
				"-p", `{"spec":{"server":{"listenPort":8282,"metricsPort":9000}}}`)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for deployment to roll out with new configuration")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.spec.template.spec.containers[0].ports[0].containerPort}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("8282"))
			}, "2m", "5s").Should(Succeed())

			By("Verifying services use custom ports")
			cmd = exec.Command("kubectl", "get", "service", lakekeeperName,
				"-n", testNamespace, "-o", "jsonpath={.spec.ports[0].port}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("8282"))

			cmd = exec.Command("kubectl", "get", "service", lakekeeperName+"-metrics",
				"-n", testNamespace, "-o", "jsonpath={.spec.ports[0].port}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("9000"))

			By("Verifying endpoints respond on custom ports")
			Eventually(func(g Gomega) {
				body, err := httpGetService(testNamespace, lakekeeperName, 8282, "/health")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(body).NotTo(BeEmpty())
			}, "2m", "5s").Should(Succeed())
		})

		It("should handle resource limits", func() {
			By("Updating Lakekeeper with resource limits")
			patchJSON := `{"spec":{"resources":{"requests":{"cpu":"100m","memory":"128Mi"},` +
				`"limits":{"cpu":"500m","memory":"512Mi"}}}}`
			cmd := exec.Command("kubectl", "patch", "lakekeeper", lakekeeperName,
				"-n", testNamespace, "--type=merge", "-p", patchJSON)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying pod has correct resources")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.spec.template.spec.containers[0].resources.limits.memory}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("512Mi"))
			}, "2m", "5s").Should(Succeed())

			By("Verifying pod is still running with resource constraints")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", "app.kubernetes.io/name=lakekeeper",
					"-o", "jsonpath={.items[0].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, "3m", "5s").Should(Succeed())
		})
	})

	Context("Lakekeeper management API", func() {
		BeforeAll(func() {
			By("Creating Lakekeeper instance for API tests")
			lakekeeperYAML := fmt.Sprintf(`
apiVersion: lakekeeper.k8s.lakekeeper.io/v1alpha1
kind: Lakekeeper
metadata:
  name: %s-api
  namespace: %s
spec:
  image: %s
%s
  authorization:
    backend: allowall
`, lakekeeperName, testNamespace, getLakekeeperImage(), defaultTestDBConfig(testNamespace))

			lakekeeperFile := filepath.Join("/tmp", fmt.Sprintf("lakekeeper-api-%s.yaml", testNamespace))
			err := os.WriteFile(lakekeeperFile, []byte(lakekeeperYAML), 0644)
			Expect(err).NotTo(HaveOccurred())

			cmd := exec.Command("kubectl", "apply", "-f", lakekeeperFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Wait for deployment to be ready
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName+"-api",
					"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, "5m", "5s").Should(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up Lakekeeper API instance")
			cmd := exec.Command("kubectl", "delete", "lakekeeper", lakekeeperName+"-api",
				"-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should expose working management API", func() {
			By("Calling /management/v1/info endpoint")
			Eventually(func(g Gomega) {
				body, err := httpGetService(testNamespace, lakekeeperName+"-api", 8181, "/management/v1/info")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(body).To(ContainSubstring("server"))
			}, "2m", "5s").Should(Succeed())
		})
	})
})

// Regression test: bootstrap detection must be unconditional.
//
// spec.bootstrap.enabled only gates whether the operator calls POST /management/v1/bootstrap.
// It does NOT gate whether the operator observes and records the bootstrap state.
//
// Even when spec.bootstrap is completely omitted (i.e. enabled=false), the operator
// MUST detect when the server becomes bootstrapped by any means and reflect that in
// status.bootstrappedAt, status.serverID, and the Bootstrapped condition.
//
// NOTE: allowall auth does NOT auto-bootstrap the server on its own. The server requires
// an explicit POST /management/v1/bootstrap call before it reports bootstrapped: true.
// This test simulates the user's manual bootstrap action via port-forward, then verifies
// that the operator correctly detects and surfaces the bootstrapped state in the CR status.
var _ = Describe("Lakekeeper Bootstrap Behavior E2E", Serial, Ordered, func() {
	var (
		testNamespace  string
		lakekeeperName string
	)

	BeforeAll(func() {
		testNamespace = "lakekeeper-nobootstrap-" + utils.RandString(8)
		lakekeeperName = "lakekeeper-nobootstrap"
		setupNamespaceWithPostgres(testNamespace)
	})

	AfterAll(func() {
		By("Deleting test namespace: " + testNamespace)
		cmd := exec.Command("kubectl", "delete", "ns", testNamespace)
		_, _ = utils.Run(cmd)
	})

	Context("when spec.bootstrap is completely omitted from the CR", func() {
		AfterEach(func() {
			By("Cleaning up Lakekeeper CR")
			cmd := exec.Command("kubectl", "delete", "lakekeeper", lakekeeperName,
				"-n", testNamespace, "--ignore-not-found=true")
			_, _ = utils.Run(cmd)
		})

		It("should detect bootstrap state even when spec.bootstrap is omitted (operator observes manual/external bootstrap)", func() { //nolint:lll
			By("Creating Lakekeeper CR with spec.bootstrap completely absent")
			// NOTE: spec.bootstrap is intentionally omitted — not even an empty object.
			// allowall auth does NOT auto-bootstrap on its own; the test simulates
			// a manual user bootstrap by calling POST /management/v1/bootstrap directly.
			// The operator must detect the bootstrap and surface it in status regardless
			// of whether spec.bootstrap.enabled is set.
			lakekeeperYAML := fmt.Sprintf(`
apiVersion: lakekeeper.k8s.lakekeeper.io/v1alpha1
kind: Lakekeeper
metadata:
  name: %s
  namespace: %s
spec:
  image: %s
  replicas: 1
%s
  authorization:
    backend: allowall
`, lakekeeperName, testNamespace, getLakekeeperImage(), defaultTestDBConfig(testNamespace))

			lakekeeperFile := filepath.Join("/tmp", fmt.Sprintf("lakekeeper-%s-nobootstrap.yaml", testNamespace))
			err := os.WriteFile(lakekeeperFile, []byte(lakekeeperYAML), 0644)
			Expect(err).NotTo(HaveOccurred())

			cmd := exec.Command("kubectl", "apply", "-f", lakekeeperFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for Lakekeeper deployment to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, "5m", "5s").Should(Succeed())

			By("Simulating manual user bootstrap via POST /management/v1/bootstrap")
			// allowall does not auto-bootstrap; we must explicitly call the bootstrap endpoint
			// to simulate what a real user would do before the operator can detect the state.
			Eventually(func(g Gomega) {
				statusCode, _, err := httpPostService(
					testNamespace,
					lakekeeperName,
					8181,
					"/management/v1/bootstrap",
					`{"accept-terms-of-use": true, "is-operator": true}`,
				)
				g.Expect(err).NotTo(HaveOccurred())
				// 204 = success, 409 = already bootstrapped (also fine)
				g.Expect(statusCode).To(BeElementOf(http.StatusNoContent, http.StatusConflict))
			}, "2m", "5s").Should(Succeed())

			By("Verifying operator detects the externally-bootstrapped server state")
			Eventually(func(g Gomega) {
				// Primary: status.bootstrappedAt must be populated.
				cmd := exec.Command("kubectl", "get", "lakekeeper", lakekeeperName,
					"-n", testNamespace,
					"-o", "jsonpath={.status.bootstrappedAt}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(),
					"status.bootstrappedAt must be set once the server is bootstrapped")

				// Secondary: Bootstrapped condition must be True.
				cmd = exec.Command("kubectl", "get", "lakekeeper", lakekeeperName,
					"-n", testNamespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Bootstrapped')].status}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"),
					"Bootstrapped condition must become True once the server is bootstrapped")

				// Tertiary: status.serverID must be populated.
				cmd = exec.Command("kubectl", "get", "lakekeeper", lakekeeperName,
					"-n", testNamespace,
					"-o", "jsonpath={.status.serverID}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty(),
					"status.serverID must be set once the server is bootstrapped")
			}, "3m", "5s").Should(Succeed())
		})
	})
})

// defaultTestDBConfig returns the YAML spec fragment for the shared test PostgreSQL database
// connection. Uses plain postgres:16-alpine without TLS (sslMode: disable), suitable only for
// the e2e test cluster. The secret "lakekeeper-db-credentials" must exist in testNamespace.
func defaultTestDBConfig(testNamespace string) string {
	return fmt.Sprintf(`  database:
    type: postgres
    postgres:
      host: postgres.%s.svc.cluster.local
      database: lakekeeper
      user: postgres
      sslMode: disable
      passwordSecretRef:
        name: lakekeeper-db-credentials
        key: password
      encryptionKeySecretRef:
        name: lakekeeper-db-credentials
        key: encryption-key`, testNamespace)
}

// httpPostService starts a temporary kubectl port-forward to the named service and performs
// an HTTP POST to path with the given JSON body, returning the status code and response body.
// doPortForwardRequest opens a kubectl port-forward tunnel to the named service,
// executes req (which must target http://localhost:<localPort>), and returns the
// response status code and body. The tunnel is torn down on return.
// localPortOffset is added to port to derive the host-side port number, allowing
// callers to avoid collisions when multiple tunnels are open simultaneously.
func doPortForwardRequest( //nolint:lll
	namespace, service string, port int32, localPortOffset int, req *http.Request,
) (int, string, error) {
	localPort := localPortOffset + int(port)
	pf := exec.Command("kubectl", "port-forward",
		fmt.Sprintf("svc/%s", service),
		fmt.Sprintf("%d:%d", localPort, port),
		"-n", namespace)
	if err := pf.Start(); err != nil {
		return 0, "", fmt.Errorf("port-forward start: %w", err)
	}
	defer func() { _ = pf.Process.Kill() }()

	// Allow a moment for the port-forward tunnel to establish.
	// The outer Eventually retries handle the case where the service is not yet ready.
	time.Sleep(500 * time.Millisecond)

	// Clone the URL to avoid mutating the caller's request.
	reqURL := *req.URL
	reqURL.Host = fmt.Sprintf("localhost:%d", localPort)
	reqURL.Scheme = "http"
	req.URL = &reqURL

	resp, err := http.DefaultClient.Do(req) //nolint:noctx
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(body), nil
}

// httpPostService port-forwards to service and performs a POST with a JSON body,
// returning the HTTP status code and response body.
func httpPostService(namespace, service string, port int32, path, jsonBody string) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, //nolint:noctx
		fmt.Sprintf("http://placeholder/%s", strings.TrimPrefix(path, "/")),
		strings.NewReader(jsonBody))
	if err != nil {
		return 0, "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doPortForwardRequest(namespace, service, port, 20000, req)
}

// httpGetService port-forwards to service and performs a GET, returning the response body.
// Returns an error for non-2xx responses. Intended for use in Eventually blocks.
func httpGetService(namespace, service string, port int32, path string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, //nolint:noctx
		fmt.Sprintf("http://placeholder/%s", strings.TrimPrefix(path, "/")),
		nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	status, body, err := doPortForwardRequest(namespace, service, port, 10000, req)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("unexpected status %d", status)
	}
	return body, nil
}

// setupNamespaceWithPostgres creates the given namespace, deploys PostgreSQL,
// waits for it to be ready, and creates the standard database credentials secret.
// It is called from BeforeAll blocks so failures abort the suite via Expect.
func setupNamespaceWithPostgres(testNamespace string) {
	By("Creating test namespace: " + testNamespace)
	cmd := exec.Command("kubectl", "create", "ns", testNamespace)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create test namespace")

	By("Deploying PostgreSQL in test namespace")
	err = deployPostgres(testNamespace)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy PostgreSQL")

	By("Waiting for PostgreSQL to be ready")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
			"-l", "app=postgres", "-o", "jsonpath={.items[0].status.phase}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("Running"))
	}, "3m", "5s").Should(Succeed())

	By("Creating database credentials secret")
	cmd = exec.Command("kubectl", "create", "secret", "generic",
		"lakekeeper-db-credentials",
		"-n", testNamespace,
		"--from-literal=password=testpassword",
		"--from-literal=encryption-key=test-encryption-key-must-be-32b!")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

// deployPostgres deploys a simple PostgreSQL StatefulSet for E2E testing
func deployPostgres(namespace string) error {
	postgresYAML := fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: %s
spec:
  ports:
  - port: 5432
  selector:
    app: postgres
  clusterIP: None
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: postgres
  namespace: %s
spec:
  serviceName: postgres
  replicas: 1
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
      - name: postgres
        image: postgres:16-alpine
        ports:
        - containerPort: 5432
        env:
        - name: POSTGRES_DB
          value: lakekeeper
        - name: POSTGRES_PASSWORD
          value: testpassword
        - name: PGDATA
          value: /var/lib/postgresql/data/pgdata
        volumeMounts:
        - name: postgres-data
          mountPath: /var/lib/postgresql/data
  volumeClaimTemplates:
  - metadata:
      name: postgres-data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 1Gi
`, namespace, namespace)

	postgresFile := filepath.Join("/tmp", fmt.Sprintf("postgres-%s.yaml", namespace))
	if err := os.WriteFile(postgresFile, []byte(postgresYAML), 0644); err != nil {
		return err
	}

	cmd := exec.Command("kubectl", "apply", "-f", postgresFile)
	_, err := utils.Run(cmd)
	return err
}

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() string {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
	return metricsOutput
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
