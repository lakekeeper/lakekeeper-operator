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
	"os"
	"testing"
)

func TestDetectClusterType_EnvironmentPriority(t *testing.T) {
	tests := []struct {
		name        string
		envVar      string
		envValue    string
		wantType    string
		wantCluster string
	}{
		{
			name:        "K3D_CLUSTER takes priority",
			envVar:      "K3D_CLUSTER",
			envValue:    "my-cluster",
			wantType:    "k3d",
			wantCluster: "my-cluster",
		},
		{
			name:        "KIND_CLUSTER takes priority",
			envVar:      "KIND_CLUSTER",
			envValue:    "my-kind-cluster",
			wantType:    ClusterTypeKind,
			wantCluster: "my-kind-cluster",
		},
		{
			name:        "MINIKUBE_PROFILE takes priority",
			envVar:      "MINIKUBE_PROFILE",
			envValue:    "test-profile",
			wantType:    ClusterTypeMinikube,
			wantCluster: "test-profile",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment variable
			_ = os.Setenv(tt.envVar, tt.envValue)
			defer func() { _ = os.Unsetenv(tt.envVar) }()

			// Call detection
			gotType, gotCluster := detectClusterType()

			// Verify environment variable took priority
			if gotType != tt.wantType {
				t.Errorf("detectClusterType() type = %v, want %v", gotType, tt.wantType)
			}
			if gotCluster != tt.wantCluster {
				t.Errorf("detectClusterType() cluster = %v, want %v", gotCluster, tt.wantCluster)
			}
		})
	}
}

func TestDetectClusterType_EnvironmentOverridesContext(t *testing.T) {
	// This test demonstrates that env vars take priority even if kubectl context
	// would auto-detect differently. In real usage, this means:
	// - Context: "my-cluster" (no pattern match)
	// - K3D_CLUSTER=my-cluster set
	// - Result: Uses k3d with cluster "my-cluster"

	// Set K3D_CLUSTER
	_ = os.Setenv("K3D_CLUSTER", "custom-name")
	defer func() { _ = os.Unsetenv("K3D_CLUSTER") }()

	gotType, gotCluster := detectClusterType()

	// Should use env var, not try context detection
	if gotType != ClusterTypeK3d {
		t.Errorf("With K3D_CLUSTER set, expected type=k3d, got %v", gotType)
	}
	if gotCluster != "custom-name" {
		t.Errorf("With K3D_CLUSTER=custom-name, expected cluster=custom-name, got %v", gotCluster)
	}
}
