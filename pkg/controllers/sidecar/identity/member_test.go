package identity

import (
	"context"
	"testing"

	scyllav1 "github.com/scylladb/scylla-operator/pkg/api/v1"
	"github.com/scylladb/scylla-operator/pkg/cmd/scylla-operator/options"
	"github.com/scylladb/scylla-operator/pkg/naming"
	"github.com/scylladb/scylla-operator/pkg/test/unit"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRetrieveRack(t *testing.T) {
	const (
		namespace     = "ns"
		podName       = "test-cluster-test-dc-test-rack-0"
		clusterName   = "test-cluster"
		nodeName      = "node-0"
		nodeRack      = "physical-rack"
		podRack       = "pod-rack"
		snitchRackVal = "snitch-rack"
	)

	tests := []struct {
		name         string
		rackFromNode bool
		podLabels    map[string]string
		// node is nil when no Node object exists
		nodeLabels    map[string]string
		withNode      bool
		expectedRack  string
		expectedError bool
	}{
		{
			name:         "snitch-rack label wins when RACK_FROM_NODE is true and the node does not exist",
			rackFromNode: true,
			podLabels:    map[string]string{naming.RackNameLabel: podRack, naming.SnitchRackLabel: snitchRackVal},
			expectedRack: snitchRackVal,
		},
		{
			name:         "snitch-rack label wins over the node label when RACK_FROM_NODE is true",
			rackFromNode: true,
			podLabels:    map[string]string{naming.RackNameLabel: podRack, naming.SnitchRackLabel: snitchRackVal},
			withNode:     true,
			nodeLabels:   map[string]string{naming.RackNameLabel: nodeRack},
			expectedRack: snitchRackVal,
		},
		{
			name:         "snitch-rack label wins when RACK_FROM_NODE is false",
			rackFromNode: false,
			podLabels:    map[string]string{naming.RackNameLabel: podRack, naming.SnitchRackLabel: snitchRackVal},
			expectedRack: snitchRackVal,
		},
		{
			name:          "empty snitch-rack label is an error",
			rackFromNode:  true,
			podLabels:     map[string]string{naming.RackNameLabel: podRack, naming.SnitchRackLabel: ""},
			withNode:      true,
			nodeLabels:    map[string]string{naming.RackNameLabel: nodeRack},
			expectedError: true,
		},
		{
			name:         "without snitch-rack label and RACK_FROM_NODE false, the pod label is used",
			rackFromNode: false,
			podLabels:    map[string]string{naming.RackNameLabel: podRack},
			withNode:     true,
			nodeLabels:   map[string]string{naming.RackNameLabel: nodeRack},
			expectedRack: podRack,
		},
		{
			name:         "without snitch-rack label and RACK_FROM_NODE true, the node label is used",
			rackFromNode: true,
			podLabels:    map[string]string{naming.RackNameLabel: podRack},
			withNode:     true,
			nodeLabels:   map[string]string{naming.RackNameLabel: nodeRack},
			expectedRack: nodeRack,
		},
		{
			name:          "without snitch-rack label and RACK_FROM_NODE true, a node without the label is an error",
			rackFromNode:  true,
			podLabels:     map[string]string{naming.RackNameLabel: podRack},
			withNode:      true,
			nodeLabels:    map[string]string{},
			expectedError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sidecarOpts := options.GetSidecarOptions()
			previous := sidecarOpts.RackFromNode
			sidecarOpts.RackFromNode = tc.rackFromNode
			defer func() { sidecarOpts.RackFromNode = previous }()

			podLabels := map[string]string{
				naming.ClusterNameLabel:    clusterName,
				naming.DatacenterNameLabel: "test-dc",
			}
			for k, v := range tc.podLabels {
				podLabels[k] = v
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: podName, Labels: podLabels},
				Spec:       corev1.PodSpec{NodeName: nodeName},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			}
			memberService := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      podName,
					Labels:    map[string]string{naming.IpLabel: "10.0.0.2"},
				},
			}
			objects := []runtime.Object{pod, memberService}
			if tc.withNode {
				objects = append(objects, &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: tc.nodeLabels},
				})
			}
			kubeClient := kubefake.NewSimpleClientset(objects...)

			scheme := runtime.NewScheme()
			if err := clientgoscheme.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := scyllav1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			cluster := unit.NewDetailedSingleRackCluster(clusterName, namespace, "repo", "2.3.1", "test-dc", "test-rack", 1)
			cc := fake.NewFakeClientWithScheme(scheme, cluster)

			member, err := Retrieve(context.Background(), podName, namespace, kubeClient, cc)
			if tc.expectedError {
				if err == nil {
					t.Fatalf("expected an error, got member %+v", member)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if member == nil {
				t.Fatal("expected a member, got nil")
			}
			if member.Rack != tc.expectedRack {
				t.Errorf("expected rack %q, got %q", tc.expectedRack, member.Rack)
			}
		})
	}
}
