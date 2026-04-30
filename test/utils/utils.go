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

package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

const (
	prometheusOperatorVersion = "v0.77.1"
	prometheusOperatorURL     = "https://github.com/prometheus-operator/prometheus-operator/" +
		"releases/download/%s/bundle.yaml"

	certmanagerVersion = "v1.16.3"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	// Cluster type constants
	ClusterTypeK3d      = "k3d"
	ClusterTypeKind     = "kind"
	ClusterTypeMinikube = "minikube"
)

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// InstallPrometheusOperator installs the prometheus Operator to be used to export the enabled metrics.
func InstallPrometheusOperator() error {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	cmd := exec.Command("kubectl", "create", "-f", url)
	_, err := Run(cmd)
	return err
}

// UninstallPrometheusOperator uninstalls the prometheus
func UninstallPrometheusOperator() {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// IsPrometheusCRDsInstalled checks if any Prometheus CRDs are installed
// by verifying the existence of key CRDs related to Prometheus.
func IsPrometheusCRDsInstalled() bool {
	// List of common Prometheus CRDs
	prometheusCRDs := []string{
		"prometheuses.monitoring.coreos.com",
		"prometheusrules.monitoring.coreos.com",
		"prometheusagents.monitoring.coreos.com",
	}

	cmd := exec.Command("kubectl", "get", "crds", "-o", "custom-columns=NAME:.metadata.name")
	output, err := Run(cmd)
	if err != nil {
		return false
	}
	crdList := GetNonEmptyLines(output)
	for _, crd := range prometheusCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err := Run(cmd)
	return err
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// LoadImageToCluster loads a local docker/podman image to the current cluster.
// It attempts to detect the cluster type (kind, k3d, minikube) and use the
// appropriate command. Users can also skip this with IMAGE_LOAD_SKIP=true.
//
// Supported cluster types:
//   - kind: Uses 'kind load docker-image'
//   - k3d: Uses 'k3d image import'
//   - minikube: Assumes image is built with minikube docker-env
//
// For cloud clusters or registries, skip this and ensure images are pushed to
// a registry accessible by the cluster.
func LoadImageToCluster(imageName string) error {
	// Try to detect cluster type from kubeconfig context
	clusterType, clusterName := detectClusterType()

	_, _ = fmt.Fprintf(GinkgoWriter, "Detected cluster type: %s (name: %s)\n", clusterType, clusterName)

	switch clusterType {
	case ClusterTypeKind:
		return loadImageToKind(imageName, clusterName)
	case ClusterTypeK3d:
		return loadImageToK3d(imageName, clusterName)
	case ClusterTypeMinikube:
		_, _ = fmt.Fprintf(GinkgoWriter, "Minikube detected. Assuming image was built with 'eval $(minikube docker-env)'\n")
		return nil // Minikube uses docker daemon directly, no load needed
	default:
		// Unknown cluster type - provide helpful message
		return fmt.Errorf("unable to detect cluster type (kind/k3d/minikube). "+
			"Current context: %s. "+
			"Please load image manually or skip with IMAGE_LOAD_SKIP=true", clusterName)
	}
}

// detectClusterType attempts to detect the cluster type from kubectl context.
// Priority order:
//  1. Environment variables (explicit user intent)
//  2. Kubectl context name patterns (auto-detection)
//  3. Return "unknown" if unable to detect
func detectClusterType() (string, string) {
	// Priority 1: Check environment variables first (explicit user intent)
	if cluster, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		_, _ = fmt.Fprintf(GinkgoWriter, "Using KIND_CLUSTER environment variable: %s\n", cluster)
		return ClusterTypeKind, cluster
	}
	if cluster, ok := os.LookupEnv("K3D_CLUSTER"); ok {
		_, _ = fmt.Fprintf(GinkgoWriter, "Using K3D_CLUSTER environment variable: %s\n", cluster)
		return "k3d", cluster
	}
	if cluster, ok := os.LookupEnv("MINIKUBE_PROFILE"); ok {
		_, _ = fmt.Fprintf(GinkgoWriter, "Using MINIKUBE_PROFILE environment variable: %s\n", cluster)
		return ClusterTypeMinikube, cluster
	}

	// Priority 2: Auto-detect from kubectl context
	cmd := exec.Command("kubectl", "config", "current-context")
	output, err := Run(cmd)
	if err != nil {
		return "unknown", ""
	}

	contextName := strings.TrimSpace(output)

	// Check for kind clusters (context usually contains 'kind-')
	if strings.Contains(contextName, "kind-") {
		// Extract cluster name: kind-<cluster-name>
		clusterName := strings.TrimPrefix(contextName, "kind-")
		return "kind", clusterName
	}

	// Check for k3d clusters (context usually is 'k3d-<cluster-name>')
	if strings.Contains(contextName, "k3d-") {
		// Extract cluster name: k3d-<cluster-name>
		clusterName := strings.TrimPrefix(contextName, "k3d-")
		return "k3d", clusterName
	}

	// Check for minikube (context is usually 'minikube')
	if contextName == "minikube" || strings.Contains(contextName, "minikube") {
		return "minikube", contextName
	}

	return "unknown", contextName
}

// loadImageToKind loads an image to a kind cluster
func loadImageToKind(imageName, clusterName string) error {
	if clusterName == "" {
		clusterName = "kind" // default
	}

	_, _ = fmt.Fprintf(GinkgoWriter, "Loading image to kind cluster '%s'...\n", clusterName)
	cmd := exec.Command("kind", "load", "docker-image", imageName, "--name", clusterName)
	_, err := Run(cmd)
	return err
}

// loadImageToK3d loads an image to a k3d cluster
func loadImageToK3d(imageName, clusterName string) error {
	if clusterName == "" {
		return fmt.Errorf("k3d cluster name not detected. Set K3D_CLUSTER environment variable")
	}

	_, _ = fmt.Fprintf(GinkgoWriter, "Loading image to k3d cluster '%s'...\n", clusterName)
	cmd := exec.Command("k3d", "image", "import", imageName, "--cluster", clusterName)
	_, err := Run(cmd)
	return err
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
// Deprecated: Use LoadImageToCluster instead for multi-cluster support
func LoadImageToKindClusterWithName(name string) error {
	return loadImageToKind(name, "")
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}

// RandString generates a random alphanumeric string of the specified length.
// This is useful for generating unique namespace names in E2E tests.
func RandString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, length)
	for i := range b {
		// nolint:gosec
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}
