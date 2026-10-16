package resource

import (
	"fmt"
	"strings"
	"testing"

	"github.com/scylladb/scylla-operator/pkg/naming"
	"github.com/scylladb/scylla-operator/pkg/test/unit"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetIpFromService(t *testing.T) {
	cluster := unit.NewSingleRackCluster(1)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "pod_service"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.10.10.10"},
	}

	for _, hostNetworking := range []bool{false, true} {
		cluster.Spec.Network.HostNetworking = hostNetworking
		t.Run(fmt.Sprintf("hostNetworking=%t", hostNetworking), func(t *testing.T) {
			service.Labels = nil
			_, err := GetIpFromService(service, cluster)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%s label not found on member service pod_service", naming.IpLabel)) {
				t.Fatalf("missing PodIP label: got error %v", err)
			}

			service.Labels = map[string]string{naming.IpLabel: "20.20.20.20"}
			ip, err := GetIpFromService(service, cluster)
			if err != nil || ip != "20.20.20.20" {
				t.Fatalf("GetIpFromService() = %q, %v; want PodIP", ip, err)
			}
		})
	}
}

func TestMemberServiceForPod_PodIPInBothNetworkModes(t *testing.T) {
	cluster := unit.NewSingleRackCluster(1)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-test-dc-test-rack-0",
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				naming.RackNameLabel:       "test-rack",
				naming.DatacenterNameLabel: "test-dc",
			},
		},
		Status: corev1.PodStatus{PodIP: "20.20.20.20"},
	}

	for _, hostNetworking := range []bool{false, true} {
		cluster.Spec.Network.HostNetworking = hostNetworking
		t.Run(fmt.Sprintf("hostNetworking=%t", hostNetworking), func(t *testing.T) {
			service := MemberServiceForPod(pod, cluster)
			if service.Spec.ClusterIP != corev1.ClusterIPNone {
				t.Errorf("ClusterIP=%q, want headless Service", service.Spec.ClusterIP)
			}
			if got := service.Labels[naming.IpLabel]; got != pod.Status.PodIP {
				t.Errorf("PodIP label=%q, want %q", got, pod.Status.PodIP)
			}
			pod.Status.PodIP = "20.20.20.21"
			service = MemberServiceForPod(pod, cluster)
			if got := service.Labels[naming.IpLabel]; got != pod.Status.PodIP {
				t.Errorf("updated PodIP label=%q, want %q", got, pod.Status.PodIP)
			}
			pod.Status.PodIP = ""
			service = MemberServiceForPod(pod, cluster)
			if _, ok := service.Labels[naming.IpLabel]; ok {
				t.Error("PodIP label present before pod has an IP")
			}
			pod.Status.PodIP = "20.20.20.20"
		})
	}
}
