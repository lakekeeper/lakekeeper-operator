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
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/lakekeeper/lakekeeper-operator/test/utils"
)

// upgradeImages returns the "from" (N) and "to" (N+1) Lakekeeper images for the
// read-only-gated upgrade spec. Both MUST be >= v0.12.3 (the first release that
// ships LAKEKEEPER__MAINTENANCE_MODE) and MUST differ so the operator runs a real
// migration. They are env-driven because the maintenance-mode feature is new and
// the test cluster needs two concrete maintenance-mode-capable tags loaded.
//
// When the env vars are absent or equal, the spec is skipped with a clear message:
// this is an environment-provisioning guard, not a way to green a real regression.
func upgradeImages() (from, to string, ok bool) {
	from = os.Getenv("LAKEKEEPER_IMAGE_FROM")
	to = os.Getenv("LAKEKEEPER_IMAGE_TO")
	if from == "" || to == "" || from == to {
		return "", "", false
	}
	return from, to, true
}

// readProbe continuously polls a read-only (GET) endpoint through a kubectl
// port-forward and counts how many responses were 503 Service Unavailable. It
// re-establishes the tunnel on transient errors (expected as pods roll), so dial
// failures are not counted — only genuine 503 responses, the maintenance-mode signal.
type readProbe struct {
	total int64
	fives int64
	stop  chan struct{}
	done  chan struct{}
}

func startReadProbe(namespace, service string, port int32, path string) *readProbe {
	p := &readProbe{stop: make(chan struct{}), done: make(chan struct{})}
	localPort := 30000 + int(port)
	url := fmt.Sprintf("http://localhost:%d%s", localPort, path)

	go func() {
		defer close(p.done)
		client := &http.Client{Timeout: 2 * time.Second}
		for {
			select {
			case <-p.stop:
				return
			default:
			}

			pf := exec.Command("kubectl", "port-forward", //nolint:gosec
				fmt.Sprintf("svc/%s", service),
				fmt.Sprintf("%d:%d", localPort, port),
				"-n", namespace)
			if err := pf.Start(); err != nil {
				time.Sleep(time.Second)
				continue
			}
			time.Sleep(500 * time.Millisecond) // let the tunnel establish

			func() {
				defer func() { _ = pf.Process.Kill() }()
				for {
					select {
					case <-p.stop:
						return
					default:
					}
					resp, err := client.Get(url) //nolint:noctx
					if err != nil {
						return // tunnel broke (pod rolled) — re-establish
					}
					_ = resp.Body.Close()
					atomic.AddInt64(&p.total, 1)
					if resp.StatusCode == http.StatusServiceUnavailable {
						atomic.AddInt64(&p.fives, 1)
					}
					time.Sleep(150 * time.Millisecond)
				}
			}()
		}
	}()

	return p
}

// stopAndReport stops the probe and returns the total number of GETs that
// completed and how many of those returned 503.
func (p *readProbe) stopAndReport() (total, fives int64) {
	close(p.stop)
	<-p.done
	return atomic.LoadInt64(&p.total), atomic.LoadInt64(&p.fives)
}

// mutatingRequest issues a mutating (POST) request to the management API and
// returns the HTTP status code and the Retry-After header. In read-only
// maintenance mode the server rejects this before handler logic with 503 +
// Retry-After; otherwise it reaches the handler and returns a non-503 status.
func mutatingRequest(namespace, service string, port int32) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, //nolint:noctx
		"http://placeholder/management/v1/warehouse",
		strings.NewReader(`{}`))
	if err != nil {
		return 0, "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// doPortForwardRequest does not surface headers, so re-implement a minimal
	// one-shot tunnel that captures Retry-After.
	localPort := 40000 + int(port)
	pf := exec.Command("kubectl", "port-forward", //nolint:gosec
		fmt.Sprintf("svc/%s", service),
		fmt.Sprintf("%d:%d", localPort, port),
		"-n", namespace)
	if err := pf.Start(); err != nil {
		return 0, "", fmt.Errorf("port-forward start: %w", err)
	}
	defer func() { _ = pf.Process.Kill() }()
	time.Sleep(500 * time.Millisecond)

	reqURL := *req.URL
	reqURL.Host = fmt.Sprintf("localhost:%d", localPort)
	reqURL.Scheme = "http"
	req.URL = &reqURL

	resp, err := http.DefaultClient.Do(req) //nolint:noctx
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, resp.Header.Get("Retry-After"), nil
}

// lakekeeperUpgradeYAML renders a Lakekeeper CR manifest for the upgrade specs.
func lakekeeperUpgradeYAML(name, namespace, image, testNamespace string, replicas int, strategy string) string {
	upgradeBlock := ""
	if strategy != "" {
		upgradeBlock = fmt.Sprintf("\n  upgrade:\n    strategy: %s", strategy)
	}
	return fmt.Sprintf(`
apiVersion: lakekeeper.k8s.lakekeeper.io/v1alpha1
kind: Lakekeeper
metadata:
  name: %s
  namespace: %s
spec:
  image: %s
  replicas: %d%s
%s
  authorization:
    backend: allowall
`, name, namespace, image, replicas, upgradeBlock, defaultTestDBConfig(testNamespace))
}

func applyLakekeeper(yaml, tmpName string) {
	file := filepath.Join("/tmp", tmpName)
	Expect(os.WriteFile(file, []byte(yaml), 0644)).To(Succeed()) //nolint:gosec
	cmd := exec.Command("kubectl", "apply", "-f", file)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

// This is the acceptance gate for VAK-448: the read-only-gated upgrade path.
// It deploys a Lakekeeper on image N, upgrades to N+1, and proves that reads stay
// available throughout while writes are blocked (503 + Retry-After) only during the
// migration window, and that state survives the upgrade.
var _ = Describe("Lakekeeper Read-Only-Gated Upgrade E2E", Serial, Ordered, func() {
	var (
		testNamespace  string
		lakekeeperName string
		imageFrom      string
		imageTo        string
	)

	BeforeAll(func() {
		var ok bool
		imageFrom, imageTo, ok = upgradeImages()
		if !ok {
			Skip("set LAKEKEEPER_IMAGE_FROM and LAKEKEEPER_IMAGE_TO to two distinct " +
				"maintenance-mode-capable (>= v0.12.3) images to run the upgrade E2E")
		}
		lakekeeperName = "lakekeeper-upgrade"
	})

	// Each Context provisions its own namespace + Postgres. They MUST NOT share a
	// database: the ReadOnlyMigration spec migrates its DB forward to imageTo, which
	// would leave an older imageFrom binary unable to migrate a shared DB.
	setupFreshDB := func(prefix string) {
		testNamespace = prefix + utils.RandString(8)
		setupNamespaceWithPostgres(testNamespace)
	}
	teardownDB := func() {
		if testNamespace != "" {
			By("Deleting test namespace: " + testNamespace)
			cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found=true")
			_, _ = utils.Run(cmd)
		}
	}

	Context("ReadOnlyMigration strategy (default)", func() {
		BeforeAll(func() { setupFreshDB("lakekeeper-upgrade-ro-") })
		AfterAll(teardownDB)

		AfterEach(func() {
			cmd := exec.Command("kubectl", "delete", "lakekeeper", lakekeeperName,
				"-n", testNamespace, "--ignore-not-found=true")
			_, _ = utils.Run(cmd)
		})

		It("keeps reads available and blocks writes only during the migration window", func() {
			By("Deploying Lakekeeper on image N with 2 replicas")
			applyLakekeeper(
				lakekeeperUpgradeYAML(lakekeeperName, testNamespace, imageFrom, testNamespace, 2, ""),
				fmt.Sprintf("lk-upgrade-%s.yaml", testNamespace))

			By("Waiting for both replicas to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("2"))
			}, "5m", "5s").Should(Succeed())

			By("Bootstrapping the server (proves writes work before the upgrade)")
			Eventually(func(g Gomega) {
				status, err := httpPostService(testNamespace, lakekeeperName, 8181,
					"/management/v1/bootstrap", `{"accept-terms-of-use": true, "is-operator": true}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(BeElementOf(http.StatusNoContent, http.StatusConflict))
			}, "2m", "5s").Should(Succeed())

			By("Recording the server-id so we can prove state survives the migration")
			var serverIDBefore string
			Eventually(func(g Gomega) {
				body, err := httpGetService(testNamespace, lakekeeperName, 8181, "/management/v1/info")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(body).To(ContainSubstring("server-id"))
				serverIDBefore = body
			}, "2m", "5s").Should(Succeed())

			By("Starting a background read probe (asserts reads never 503)")
			probe := startReadProbe(testNamespace, lakekeeperName, 8181, "/management/v1/info")

			By("Patching spec.image to N+1 to trigger the upgrade")
			cmd := exec.Command("kubectl", "patch", "lakekeeper", lakekeeperName,
				"-n", testNamespace, "--type=merge",
				"-p", fmt.Sprintf(`{"spec":{"image":%q}}`, imageTo))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("Observing status.upgradePhase transition through the choreography")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lakekeeper", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.status.upgradePhase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(BeElementOf("Quiescing", "Migrating", "RollingOut"))
			}, "2m", "2s").Should(Succeed())

			By("Asserting a write returns 503 + Retry-After during the read-only window")
			Eventually(func(g Gomega) {
				status, retryAfter, err := mutatingRequest(testNamespace, lakekeeperName, 8181)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal(http.StatusServiceUnavailable))
				g.Expect(retryAfter).NotTo(BeEmpty(), "read-only mode must set a Retry-After header")
			}, "3m", "2s").Should(Succeed())

			By("Waiting for the upgrade to complete (upgradePhase cleared, Ready=True)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lakekeeper", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.status.upgradePhase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(BeEmpty())

				cmd = exec.Command("kubectl", "get", "lakekeeper", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}, "10m", "5s").Should(Succeed())

			By("Verifying the pods now run image N+1")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName,
					"-n", testNamespace, "-o", "jsonpath={.spec.template.spec.containers[0].image}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(imageTo))
			}, "2m", "5s").Should(Succeed())

			By("Stopping the read probe and asserting it never saw a 503")
			total, fives := probe.stopAndReport()
			Expect(total).To(BeNumerically(">", 0), "the read probe should have made requests")
			Expect(fives).To(BeZero(), "reads must never be rejected with 503 during the upgrade")

			By("Asserting writes work again after the upgrade")
			Eventually(func(g Gomega) {
				status, _, err := mutatingRequest(testNamespace, lakekeeperName, 8181)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).NotTo(Equal(http.StatusServiceUnavailable))
			}, "2m", "5s").Should(Succeed())

			By("Asserting the bootstrapped state survived the migration (same server-id)")
			Eventually(func(g Gomega) {
				body, err := httpGetService(testNamespace, lakekeeperName, 8181, "/management/v1/info")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(body).To(Equal(serverIDBefore))
			}, "2m", "5s").Should(Succeed())
		})
	})

	Context("Simple strategy (legacy opt-out)", func() {
		BeforeAll(func() { setupFreshDB("lakekeeper-upgrade-simple-") })
		AfterAll(teardownDB)

		AfterEach(func() {
			cmd := exec.Command("kubectl", "delete", "lakekeeper", lakekeeperName+"-simple",
				"-n", testNamespace, "--ignore-not-found=true")
			_, _ = utils.Run(cmd)
		})

		It("never enters the read-only window", func() {
			By("Deploying Lakekeeper on image N with strategy: Simple")
			applyLakekeeper(
				lakekeeperUpgradeYAML(lakekeeperName+"-simple", testNamespace, imageFrom, testNamespace, 2, "Simple"),
				fmt.Sprintf("lk-upgrade-simple-%s.yaml", testNamespace))

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName+"-simple",
					"-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("2"))
			}, "5m", "5s").Should(Succeed())

			By("Patching spec.image to N+1")
			cmd := exec.Command("kubectl", "patch", "lakekeeper", lakekeeperName+"-simple",
				"-n", testNamespace, "--type=merge",
				"-p", fmt.Sprintf(`{"spec":{"image":%q}}`, imageTo))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("Asserting upgradePhase never becomes non-empty (no read-only gating)")
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lakekeeper", lakekeeperName+"-simple",
					"-n", testNamespace, "-o", "jsonpath={.status.upgradePhase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(BeEmpty())
			}, "90s", "3s").Should(Succeed())

			By("Waiting for the Simple upgrade to converge to image N+1")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", lakekeeperName+"-simple",
					"-n", testNamespace, "-o", "jsonpath={.spec.template.spec.containers[0].image}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(imageTo))
			}, "10m", "5s").Should(Succeed())
		})
	})
})
