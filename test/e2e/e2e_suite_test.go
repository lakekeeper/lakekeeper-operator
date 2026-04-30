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
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/lakekeeper/lakekeeper-operator/test/utils"
)

var (
	// Optional Environment Variables:
	// - CERT_MANAGER_INSTALL_SKIP=true: Skips CertManager installation during test setup.
	// - IMAGE_LOAD_SKIP=true: Skips automatic image loading to the cluster.
	//   Use this when testing with k3d, minikube, or when images are already available.
	//   Users must ensure images are available in their cluster before running tests.
	// These variables are useful if CertManager is already installed or if you're managing
	// image availability manually.
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true"
	skipImageLoad          = os.Getenv("IMAGE_LOAD_SKIP") == "true"
	// isCertManagerAlreadyInstalled will be set true when CertManager CRDs be found on the cluster
	isCertManagerAlreadyInstalled = false

	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectImage = "example.com/lakekeeper-operator:v0.0.1"
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purposed to be used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting lakekeeper-operator integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the manager(Operator) image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	// Load the image to the cluster if not skipped
	// This works with Kind, k3d, minikube, and other local clusters
	// Skip with IMAGE_LOAD_SKIP=true if you've already loaded the image manually
	// or if you're using a registry that the cluster can pull from
	if !skipImageLoad {
		By("loading the manager(Operator) image to the cluster")
		err = utils.LoadImageToCluster(projectImage)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: Failed to load image automatically: %v\n", err)
			_, _ = fmt.Fprintf(GinkgoWriter, "Please ensure the image '%s' is available in your cluster.\n", projectImage)
			_, _ = fmt.Fprintf(GinkgoWriter, "You can:\n")
			_, _ = fmt.Fprintf(GinkgoWriter,
				"  - For kind: kind load docker-image %s --name <cluster-name>\n", projectImage)
			_, _ = fmt.Fprintf(GinkgoWriter,
				"  - For k3d: k3d image import %s --cluster <cluster-name>\n", projectImage)
			_, _ = fmt.Fprintf(GinkgoWriter, "  - For minikube: eval $(minikube docker-env) && docker build ...\n")
			_, _ = fmt.Fprintf(GinkgoWriter, "  - Or skip this step with IMAGE_LOAD_SKIP=true\n")
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image to cluster")
		}
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter,
			"Skipping image load (IMAGE_LOAD_SKIP=true). Ensure image '%s' is available in cluster.\n",
			projectImage)
	}

	// The tests-e2e are intended to run on a temporary cluster that is created and destroyed for testing.
	// To prevent errors when tests run in environments with CertManager already installed,
	// we check for its presence before execution.
	// Setup CertManager before the suite if not skipped and if not already installed
	if !skipCertManagerInstall {
		By("checking if cert manager is installed already")
		isCertManagerAlreadyInstalled = utils.IsCertManagerCRDsInstalled()
		if !isCertManagerAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing CertManager...\n")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CertManager is already installed. Skipping installation...\n")
		}
	}

	// Install CRDs globally so they're available for all test Describe blocks
	By("installing Lakekeeper CRDs")
	cmd = exec.Command("make", "install")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install Lakekeeper CRDs")

	// Verify CRDs are registered
	By("verifying Lakekeeper CRDs are registered")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "crd", "lakekeepers.lakekeeper.k8s.lakekeeper.io")
		_, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "Lakekeeper CRD not found after installation")
	}, "30s", "1s").Should(Succeed())

	// Deploy controller for entire suite - it needs to stay running for all tests
	// (Manager tests, Lakekeeper tests, etc.)
	By("creating operator namespace")
	cmd = exec.Command("kubectl", "create", "ns", "lakekeeper-operator-system")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to create operator namespace")

	By("labeling operator namespace with restricted policy")
	cmd = exec.Command("kubectl", "label", "--overwrite", "ns", "lakekeeper-operator-system",
		"pod-security.kubernetes.io/enforce=restricted")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to label namespace")

	By("deploying the controller-manager for entire suite")
	cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy controller-manager")

	By("waiting for controller-manager to be ready")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "deployment",
			"lakekeeper-operator-controller-manager",
			"-n", "lakekeeper-operator-system",
			"-o", "jsonpath={.status.readyReplicas}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("1"))
	}, "2m", "5s").Should(Succeed())
})

var _ = AfterSuite(func() {
	// Undeploy controller-manager deployed in BeforeSuite
	By("undeploying controller-manager")
	cmd := exec.Command("kubectl", "delete", "deployment",
		"lakekeeper-operator-controller-manager",
		"-n", "lakekeeper-operator-system",
		"--ignore-not-found=true")
	_, _ = utils.Run(cmd)

	By("deleting operator namespace")
	cmd = exec.Command("kubectl", "delete", "ns", "lakekeeper-operator-system",
		"--ignore-not-found=true")
	_, _ = utils.Run(cmd)

	// Uninstall CRDs at the end of the entire suite
	By("uninstalling Lakekeeper CRDs")
	cmd = exec.Command("make", "uninstall")
	_, _ = utils.Run(cmd)

	// Clean up cluster-scoped RBAC resources created by operator deployment
	// These are not deleted by namespace cleanup and must be explicitly removed
	By("cleaning up operator ClusterRoleBindings")
	cmd = exec.Command("kubectl", "delete", "clusterrolebinding",
		"lakekeeper-operator-manager-rolebinding",
		"lakekeeper-operator-metrics-auth-rolebinding",
		"--ignore-not-found=true")
	_, _ = utils.Run(cmd)

	By("cleaning up operator ClusterRoles")
	cmd = exec.Command("kubectl", "delete", "clusterrole",
		"lakekeeper-operator-manager-role",
		"lakekeeper-operator-metrics-reader",
		"lakekeeper-operator-metrics-auth-role",
		"lakekeeper-operator-lakekeeper-admin-role",
		"lakekeeper-operator-lakekeeper-editor-role",
		"lakekeeper-operator-lakekeeper-viewer-role",
		"--ignore-not-found=true")
	_, _ = utils.Run(cmd)

	// Teardown CertManager after the suite if not skipped and if it was not already installed
	if !skipCertManagerInstall && !isCertManagerAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling CertManager...\n")
		utils.UninstallCertManager()
	}
})
