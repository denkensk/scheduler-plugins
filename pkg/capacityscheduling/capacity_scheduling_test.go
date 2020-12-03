/*
Copyright 2020 The Kubernetes Authors.

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

package capacityscheduling

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	dp "k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultpreemption"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

const ResourceGPU v1.ResourceName = "nvidia.com/gpu"

var (
	negPriority, lowPriority, midPriority, highPriority, veryHighPriority = int32(-100), int32(0), int32(100), int32(1000), int32(10000)

	smallRes = map[v1.ResourceName]string{
		v1.ResourceCPU:    "100m",
		v1.ResourceMemory: "100",
	}
	mediumRes = map[v1.ResourceName]string{
		v1.ResourceCPU:    "200m",
		v1.ResourceMemory: "200",
	}
	largeRes = map[v1.ResourceName]string{
		v1.ResourceCPU:    "300m",
		v1.ResourceMemory: "300",
	}
	veryLargeRes = map[v1.ResourceName]string{
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}

	epochTime  = metav1.NewTime(time.Unix(0, 0))
	epochTime1 = metav1.NewTime(time.Unix(0, 1))
	epochTime2 = metav1.NewTime(time.Unix(0, 2))
	epochTime3 = metav1.NewTime(time.Unix(0, 3))
	epochTime4 = metav1.NewTime(time.Unix(0, 4))
	epochTime5 = metav1.NewTime(time.Unix(0, 5))
	epochTime6 = metav1.NewTime(time.Unix(0, 6))
)

func TestPreFilter(t *testing.T) {
	type podInfo struct {
		podName      string
		podNamespace string
		memReq       int64
	}

	tests := []struct {
		name          string
		podInfos      []podInfo
		elasticQuotas map[string]*ElasticQuotaInfo
		expected      []framework.Code
	}{
		{
			name: "pod belongs ElasticQuota",
			podInfos: []podInfo{
				{podName: "ns1-p1", podNamespace: "ns1", memReq: 500},
				{podName: "ns1-p2", podNamespace: "ns1", memReq: 1500},
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Min: &framework.Resource{
						Memory: 1000,
					},
					Max: &framework.Resource{
						Memory: 2000,
					},
					Used: &framework.Resource{
						Memory: 800,
					},
				},
			},
			expected: []framework.Code{
				framework.Success,
				framework.UnschedulableAndUnresolvable,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := &CapacityScheduling{
				elasticQuotaInfos: tt.elasticQuotas,
			}

			pods := make([]*v1.Pod, 0)
			for _, podInfo := range tt.podInfos {
				pod := makePods(podInfo.podName, podInfo.podNamespace, podInfo.memReq, 0, 0, 0, podInfo.podName, "")
				pods = append(pods, pod)
			}

			state := framework.NewCycleState()
			for i := range pods {
				if got := cs.PreFilter(nil, state, pods[i]); got.Code() != tt.expected[i] {
					t.Errorf("expected %v, got %v", tt.expected, got.Code())
				}
			}
		})
	}
}

func TestFindCandidates(t *testing.T) {
	res := map[v1.ResourceName]string{v1.ResourceMemory: "150"}
	tests := []struct {
		name          string
		pod           *v1.Pod
		pods          []*v1.Pod
		nodes         []*v1.Node
		nodesStatuses framework.NodeToStatusMap
		elasticQuotas map[string]*ElasticQuotaInfo
		want          []dp.Candidate
	}{
		{
			name: "intra namespace preempt",
			pod:  makePods("t1-p", "ns1", 50, 0, 0, highPriority, "", "t1-p"),
			pods: []*v1.Pod{
				makePods("t1-p1", "ns1", 50, 0, 0, midPriority, "t1-p1", "node-a"),
				makePods("t1-p2", "ns2", 50, 0, 0, midPriority, "t1-p2", "node-a"),
				makePods("t1-p3", "ns2", 50, 0, 0, midPriority, "t1-p3", "node-a"),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 50,
					},
				},
				"ns2": {
					Namespace: "ns2",
					Min: &framework.Resource{
						Memory: 100,
					},
					Used: &framework.Resource{
						Memory: 100,
					},
				},
			},
			nodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
			},
			want: []dp.Candidate{
				&candidate{
					victims: &extenderv1.Victims{
						Pods: []*v1.Pod{
							makePods("t1-p1", "ns1", 50, 0, 0, midPriority, "t1-p1", "node-a"),
						},
						NumPDBViolations: 0,
					},
					name: "node-a",
				},
			},
		},
		{
			name: "inter namespace preempt",
			pod:  makePods("t1-p", "ns1", 50, 0, 0, highPriority, "", "t1-p"),
			pods: []*v1.Pod{
				makePods("t1-p1", "ns1", 50, 0, 0, midPriority, "t1-p1", "node-a"),
				makePods("t1-p2", "ns2", 50, 0, 0, highPriority, "t1-p2", "node-a"),
				makePods("t1-p3", "ns2", 50, 0, 0, midPriority, "t1-p3", "node-a"),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Max: &framework.Resource{
						Memory: 200,
					},
					Min: &framework.Resource{
						Memory: 100,
					},
					Used: &framework.Resource{
						Memory: 50,
					},
				},
				"ns2": {
					Namespace: "ns2",
					Max: &framework.Resource{
						Memory: 200,
					},
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 100,
					},
				},
			},
			nodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
			},
			want: []dp.Candidate{
				&candidate{
					victims: &extenderv1.Victims{
						Pods: []*v1.Pod{
							makePods("t1-p3", "ns2", 50, 0, 0, midPriority, "t1-p3", "node-a"),
						},
						NumPDBViolations: 0,
					},
					name: "node-a",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registeredPlugins := []st.RegisterPluginFunc{
				st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				st.RegisterPluginAsExtensions(noderesources.FitName, noderesources.NewFit, "Filter", "PreFilter"),
			}

			cs := clientsetfake.NewSimpleClientset()
			fwk, err := st.NewFramework(
				registeredPlugins,
				frameworkruntime.WithClientSet(cs),
				frameworkruntime.WithEventRecorder(&events.FakeRecorder{}),
				frameworkruntime.WithPodNominator(testutil.NewPodNominator()),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(tt.pods, tt.nodes)),
				frameworkruntime.WithInformerFactory(informers.NewSharedInformerFactory(cs, 0)),
			)
			if err != nil {
				t.Fatal(err)
			}

			state := framework.NewCycleState()
			ctx := context.Background()

			// Some tests rely on PreFilter plugin to compute its CycleState.
			preFilterStatus := fwk.RunPreFilterPlugins(ctx, state, tt.pod)
			if !preFilterStatus.IsSuccess() {
				t.Errorf("Unexpected preFilterStatus: %v", preFilterStatus)
			}

			prefilterStatue := computePodResourceRequest(tt.pod)
			elasticQuotaSnapshotState := &ElasticQuotaSnapshotState{
				elasticQuotaInfos: tt.elasticQuotas,
			}
			state.Write(preFilterStateKey, prefilterStatue)
			state.Write(ElasticQuotaSnapshotKey, elasticQuotaSnapshotState)

			got, err := FindCandidates(ctx, cs, state, tt.pod, tt.nodesStatuses, fwk.PreemptHandle(), fwk.SnapshotSharedLister().NodeInfos(), getPDBLister(fwk.SharedInformerFactory()))
			if err != nil {
				t.Fatal(err)
			}

			// Sort the values (inner victims) and the candidate itself (by its NominatedNodeName).
			for i := range got {
				victims := got[i].Victims().Pods
				sort.Slice(victims, func(i, j int) bool {
					return victims[i].Name < victims[j].Name
				})
			}
			sort.Slice(got, func(i, j int) bool {
				return got[i].Name() < got[j].Name()
			})
			if diff := cmp.Diff(tt.want, got, cmp.AllowUnexported(candidate{})); diff != "" {
				t.Errorf("Unexpected candidates (-want, +got): %s", diff)
			}
		})
	}
}

func makePods(podName string, namespace string, memReq int64, cpuReq int64, gpuReq int64, priority int32, uid string, nodeName string) *v1.Pod {
	pause := imageutils.GetPauseImageName()
	pod := st.MakePod().Namespace(namespace).Name(podName).Container(pause).
		Priority(priority).Node(nodeName).UID(uid).ZeroTerminationGracePeriod().Obj()
	pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
		Requests: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(memReq, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewMilliQuantity(cpuReq, resource.DecimalSI),
			ResourceGPU:       *resource.NewQuantity(gpuReq, resource.DecimalSI),
		},
	}
	return pod
}