/*
Copyright 2024.

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

package metrics

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	modelsCommon "github.com/kubeflow/notebooks/workspaces/backend/internal/models/common"
	models "github.com/kubeflow/notebooks/workspaces/backend/internal/models/workspaces/podtemplate/resources"
	repoCommon "github.com/kubeflow/notebooks/workspaces/backend/internal/repositories/common"

	kubefloworgv1beta1 "github.com/kubeflow/notebooks/workspaces/controller/api/v1beta1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/cache"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestMetricsRepository(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Metrics Repository")
}

var _ = Describe("MetricsRepository.GetWorkspaceResourceUsage", func() {
	var (
		scheme *runtime.Scheme
		ctx    context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(metricsv1beta1.AddToScheme(scheme)).To(Succeed())
		Expect(kubefloworgv1beta1.AddToScheme(scheme)).To(Succeed())
	})

	It("returns available usage joined with requests when pods and metrics exist", func() {
		pod := workspacePod("pod-1", "container-1", corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		})
		metrics := workspacePodMetrics("pod-1", "container-1", corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		})

		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testWorkspaceCR(), metrics).
			WithLists(&corev1.PodList{Items: []corev1.Pod{*pod}}).
			Build()

		repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)
		got, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
		expected := models.ContainerResourceUsage{
			MetricsFromMetricsServer: &models.MetricsFromMetricsServer{
				Timestamp: "0001-01-01T00:00:00Z",
				Usage: models.ResourceValues{
					CPU:    "50m",
					Memory: "100Mi",
				},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			},
		}

		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
		Expect(got.Containers).To(HaveLen(1))
		Expect(got.Containers["container-1"]).To(BeComparableTo(expected))
	})

	It("omits metricsFromMetricsServer when PodMetrics is missing", func() {
		pod := workspacePod("pod-2", "container-2", corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		})

		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testWorkspaceCR()).
			WithLists(&corev1.PodList{Items: []corev1.Pod{*pod}}).
			Build()

		repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)
		got, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
		expected := models.ContainerResourceUsage{
			MetricsFromMetricsServer: nil,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			},
		}

		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
		Expect(got.Containers).To(HaveLen(1))
		Expect(got.Containers["container-2"]).To(BeComparableTo(expected))
	})

	It("returns ErrWorkspaceNotFound when the workspace does not exist", func() {
		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testWorkspaceCR()).
			WithLists(&corev1.PodList{Items: []corev1.Pod{}}).
			Build()

		repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)
		got, err := repo.GetWorkspaceResourceUsage(ctx, "default", "no-such-workspace")

		Expect(err).To(MatchError(repoCommon.ErrWorkspaceNotFound))
		Expect(got).To(BeNil())
	})

	It("returns ErrWorkspacePodNotRunning when no pods match", func() {
		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testWorkspaceCR()).
			WithLists(&corev1.PodList{Items: []corev1.Pod{}}).
			Build()

		repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)
		got, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")

		Expect(err).To(MatchError(repoCommon.ErrWorkspacePodNotRunning))
		Expect(got).To(BeNil())
	})

	It("omits metricsFromMetricsServer when API is not served", func() {
		pod := workspacePod("pod-3", "container-3", corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		})

		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testWorkspaceCR()).
			WithLists(&corev1.PodList{Items: []corev1.Pod{*pod}}).
			Build()

		repo := newTestMetricsRepository(cli, false, resourceUsageCacheMaxCapacity)
		got, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
		expected := models.ContainerResourceUsage{
			MetricsFromMetricsServer: nil,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			},
		}

		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
		Expect(got.Containers).To(HaveLen(1))
		Expect(got.Containers["container-3"]).To(BeComparableTo(expected))
	})

	It("omits metricsFromMetricsServer when PodMetrics returns NotFound", func() {
		pod := workspacePod("pod-4", "container-4", corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		})

		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testWorkspaceCR()).
			WithLists(&corev1.PodList{Items: []corev1.Pod{*pod}}).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isMetrics := obj.(*metricsv1beta1.PodMetrics); isMetrics {
						return apierrors.NewNotFound(schema.GroupResource{Group: metricsv1beta1.GroupName, Resource: "podmetrics"}, "")
					}
					return cli.Get(ctx, key, obj, opts...)
				},
			}).
			Build()

		repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)
		got, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
		expected := models.ContainerResourceUsage{
			MetricsFromMetricsServer: nil,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			},
		}

		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
		Expect(got.Containers).To(HaveLen(1))
		Expect(got.Containers["container-4"]).To(BeComparableTo(expected))
	})

	It("omits metricsFromMetricsServer when PodMetrics returns ServiceUnavailable", func() {
		pod := workspacePod("pod-4", "container-4", corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		})

		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testWorkspaceCR()).
			WithLists(&corev1.PodList{Items: []corev1.Pod{*pod}}).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isMetrics := obj.(*metricsv1beta1.PodMetrics); isMetrics {
						return apierrors.NewServiceUnavailable("metrics service unavailable")
					}
					return cli.Get(ctx, key, obj, opts...)
				},
			}).
			Build()

		repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)
		got, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
		expected := models.ContainerResourceUsage{
			MetricsFromMetricsServer: nil,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			},
		}

		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
		Expect(got.Containers).To(HaveLen(1))
		Expect(got.Containers["container-4"]).To(BeComparableTo(expected))
	})

	It("omits metricsFromMetricsServer when getting PodMetrics is forbidden", func() {
		pod := workspacePod("pod-4", "container-4", corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		})

		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testWorkspaceCR()).
			WithLists(&corev1.PodList{Items: []corev1.Pod{*pod}}).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isMetrics := obj.(*metricsv1beta1.PodMetrics); isMetrics {
						return apierrors.NewForbidden(schema.GroupResource{}, "", errors.New("forbidden"))
					}
					return cli.Get(ctx, key, obj, opts...)
				},
			}).
			Build()

		repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)
		got, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
		expected := models.ContainerResourceUsage{
			MetricsFromMetricsServer: nil,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			},
		}

		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
		Expect(got.Containers).To(HaveLen(1))
		Expect(got.Containers["container-4"]).To(BeComparableTo(expected))
	})

	It("propagates context cancellation when the caller's context is cancelled before the fetch completes", func() {
		// The miss-path fetch runs on a context detached from the caller's
		// via context.WithoutCancel, but the caller's select-loop still
		// honours its own ctx.Done() — so the caller sees ctx.Err() even
		// though the fetch continues (harmlessly) in the background.
		pod := workspacePod("pod-6", "container-6", corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		})

		blockFetch := make(chan struct{})
		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testWorkspaceCR()).
			WithLists(&corev1.PodList{Items: []corev1.Pod{*pod}}).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isMetrics := obj.(*metricsv1beta1.PodMetrics); isMetrics {
						<-blockFetch
						return context.Canceled
					}
					return cli.Get(ctx, key, obj, opts...)
				},
			}).
			Build()

		callerCtx, cancel := context.WithCancel(context.Background())
		repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)

		errCh := make(chan error, 1)
		go func() {
			_, err := repo.GetWorkspaceResourceUsage(callerCtx, "default", "test-workspace")
			errCh <- err
		}()

		// Cancel the caller while the fetch is still blocked. The caller's
		// select should immediately return ctx.Err().
		cancel()
		Eventually(errCh).Should(Receive(MatchError(context.Canceled)))

		// Release the interceptor so the background fetch drains cleanly.
		close(blockFetch)
	})

	Context("resource usage cache — hit path", func() {
		It("serves the cache-hit response without touching Kubernetes (zero informer reads) — review finding #6", func() {
			pod := workspacePod("pod-hit", "container-hit", corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"),
			})
			metrics := workspacePodMetrics("pod-hit", "container-hit", corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("75m"),
			})

			var workspaceGetCount, podListCount, podMetricsGetCount int32
			cli := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(testWorkspaceCR(), metrics).
				WithLists(&corev1.PodList{Items: []corev1.Pod{*pod}}).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						switch obj.(type) {
						case *kubefloworgv1beta1.Workspace:
							atomic.AddInt32(&workspaceGetCount, 1)
						case *metricsv1beta1.PodMetrics:
							atomic.AddInt32(&podMetricsGetCount, 1)
						}
						return cli.Get(ctx, key, obj, opts...)
					},
					List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if _, isPodList := list.(*corev1.PodList); isPodList {
							atomic.AddInt32(&podListCount, 1)
						}
						return cli.List(ctx, list, opts...)
					},
				}).
				Build()

			repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)

			// First call — cold cache, all three reads expected.
			first, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
			Expect(err).NotTo(HaveOccurred())
			Expect(first).NotTo(BeNil())
			Expect(atomic.LoadInt32(&workspaceGetCount)).To(Equal(int32(1)))
			Expect(atomic.LoadInt32(&podListCount)).To(Equal(int32(1)))
			Expect(atomic.LoadInt32(&podMetricsGetCount)).To(Equal(int32(1)))

			// Second call — cache hit, all counters should stay unchanged.
			second, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
			Expect(err).NotTo(HaveOccurred())
			Expect(second).To(BeIdenticalTo(first))
			Expect(atomic.LoadInt32(&workspaceGetCount)).To(Equal(int32(1)))
			Expect(atomic.LoadInt32(&podListCount)).To(Equal(int32(1)))
			Expect(atomic.LoadInt32(&podMetricsGetCount)).To(Equal(int32(1)))
		})

		It("serves cached usage from a prior pod for up to the positive TTL after pod recreation (accepted trade-off, review finding #6)", func() {
			// This test PINS the design trade-off: dropping podUID from the
			// cache key eliminates informer copies from the hit path in
			// exchange for bounded staleness on pod recreation. If the
			// design changes (e.g., re-adding podUID to the cache key),
			// update this test deliberately, not silently.
			pod1 := workspacePod("pod-1", "container-1", corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"),
			})
			pod1.UID = types.UID("uid-pod-1")
			metrics1 := workspacePodMetrics("pod-1", "container-1", corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("50m"),
			})

			pod2 := workspacePod("pod-1", "container-1", corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("200m"),
			})
			pod2.UID = types.UID("uid-pod-2")
			metrics2 := workspacePodMetrics("pod-1", "container-1", corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("150m"),
			})

			fakeCli := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(testWorkspaceCR(), pod1, metrics1).
				Build()

			repo := newTestMetricsRepository(fakeCli, true, resourceUsageCacheMaxCapacity)

			// First query populates the cache with pod1's snapshot.
			first, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
			Expect(err).NotTo(HaveOccurred())
			Expect(first.Containers["container-1"].Resources.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("100m")))
			Expect(first.Containers["container-1"].MetricsFromMetricsServer.Usage.CPU).To(Equal("50m"))

			// Simulate pod recreation with new UID and new resource spec.
			Expect(fakeCli.Delete(ctx, pod1)).To(Succeed())
			Expect(fakeCli.Create(ctx, pod2)).To(Succeed())
			Expect(fakeCli.Delete(ctx, metrics1)).To(Succeed())
			Expect(fakeCli.Create(ctx, metrics2)).To(Succeed())

			// Immediate follow-up query serves pod1's cached snapshot even
			// though the underlying pod has changed. This is intentional.
			second, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
			Expect(err).NotTo(HaveOccurred())
			Expect(second).To(BeIdenticalTo(first))
			Expect(second.Containers["container-1"].Resources.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("100m")))
			Expect(second.Containers["container-1"].MetricsFromMetricsServer.Usage.CPU).To(Equal("50m"))

			// After the positive TTL expires, the next query re-fetches
			// and picks up pod2. We force expiry by removing the entry
			// rather than sleeping 30 s in a unit test.
			repo.usageCache.Remove("default/test-workspace")
			third, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
			Expect(err).NotTo(HaveOccurred())
			Expect(third.Containers["container-1"].Resources.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("200m")))
			Expect(third.Containers["container-1"].MetricsFromMetricsServer.Usage.CPU).To(Equal("150m"))
		})
	})

	Context("resource usage cache — miss path", func() {
		It("coalesces concurrent misses so only one PodMetrics fetch runs per key (review finding #1)", func() {
			pod := workspacePod("pod-sf", "container-sf", corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"),
			})
			metrics := workspacePodMetrics("pod-sf", "container-sf", corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("50m"),
			})

			var podMetricsGetCount int32
			release := make(chan struct{})
			cli := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(testWorkspaceCR(), metrics).
				WithLists(&corev1.PodList{Items: []corev1.Pod{*pod}}).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, isMetrics := obj.(*metricsv1beta1.PodMetrics); isMetrics {
							atomic.AddInt32(&podMetricsGetCount, 1)
							<-release
						}
						return cli.Get(ctx, key, obj, opts...)
					},
				}).
				Build()

			repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)

			const N = 20
			var (
				wg      sync.WaitGroup
				results = make([]*models.WorkspaceResourceUsage, N)
				errs    = make([]error, N)
			)
			for i := 0; i < N; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					results[i], errs[i] = repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
				}(i)
			}

			// Give the goroutines time to fan out and all attempt the miss.
			// The single in-flight leader is now blocked in the interceptor.
			Eventually(func() int32 { return atomic.LoadInt32(&podMetricsGetCount) }).Should(Equal(int32(1)))
			Consistently(func() int32 { return atomic.LoadInt32(&podMetricsGetCount) }, 100*time.Millisecond, 10*time.Millisecond).Should(Equal(int32(1)))

			close(release)
			wg.Wait()

			// Every caller received the same snapshot with no errors.
			Expect(atomic.LoadInt32(&podMetricsGetCount)).To(Equal(int32(1)))
			for i := 0; i < N; i++ {
				Expect(errs[i]).NotTo(HaveOccurred())
				Expect(results[i]).To(BeIdenticalTo(results[0]))
			}
		})

		It("caches degraded responses briefly so warm-up polling does not hammer Metrics Server (review finding #4)", func() {
			pod := workspacePod("pod-warm", "container-warm", corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"),
			})

			var podMetricsGetCount int32
			cli := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(testWorkspaceCR()).
				WithLists(&corev1.PodList{Items: []corev1.Pod{*pod}}).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, isMetrics := obj.(*metricsv1beta1.PodMetrics); isMetrics {
							atomic.AddInt32(&podMetricsGetCount, 1)
							return apierrors.NewNotFound(schema.GroupResource{Group: metricsv1beta1.GroupName, Resource: "podmetrics"}, "")
						}
						return cli.Get(ctx, key, obj, opts...)
					},
				}).
				Build()

			repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)

			// First call — miss, populates negative cache entry.
			first, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
			Expect(err).NotTo(HaveOccurred())
			Expect(first.Containers["container-warm"].MetricsFromMetricsServer).To(BeNil())
			Expect(atomic.LoadInt32(&podMetricsGetCount)).To(Equal(int32(1)))

			// Immediate follow-up — served from negative cache, no fetch.
			second, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
			Expect(err).NotTo(HaveOccurred())
			Expect(second).To(BeIdenticalTo(first))
			Expect(atomic.LoadInt32(&podMetricsGetCount)).To(Equal(int32(1)))

			// Confirm the entry lives in usageCache and is a cachedUsage.
			val, ok := repo.usageCache.Get("default/test-workspace")
			Expect(ok).To(BeTrue())
			Expect(val).To(BeAssignableToTypeOf(&cachedUsage{}))
		})

		It("caches degraded responses when PodMetrics has no container metrics (review finding #4)", func() {
			pod := workspacePod("pod-empty", "container-empty", corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"),
			})
			emptyMetrics := &metricsv1beta1.PodMetrics{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-empty", Namespace: "default"},
				Containers: []metricsv1beta1.ContainerMetrics{},
			}
			cli := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(testWorkspaceCR(), pod, emptyMetrics).
				Build()

			repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)

			got, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Containers["container-empty"].MetricsFromMetricsServer).To(BeNil())

			val, ok := repo.usageCache.Get("default/test-workspace")
			Expect(ok).To(BeTrue())
			Expect(val).To(BeAssignableToTypeOf(&cachedUsage{}))
		})

		It("bounds the Metrics Server call and returns a degraded response quickly when the upstream is wedged (review finding #7)", func() {
			pod := workspacePod("pod-slow", "container-slow", corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"),
			})

			cli := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(testWorkspaceCR()).
				WithLists(&corev1.PodList{Items: []corev1.Pod{*pod}}).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, isMetrics := obj.(*metricsv1beta1.PodMetrics); isMetrics {
							// Block on the bounded ctx that fetchPodMetrics
							// derives via context.WithTimeout(metricsServerCallTimeout).
							<-ctx.Done()
							return ctx.Err()
						}
						return cli.Get(ctx, key, obj, opts...)
					},
				}).
				Build()

			repo := newTestMetricsRepository(cli, true, resourceUsageCacheMaxCapacity)

			start := time.Now()
			got, err := repo.GetWorkspaceResourceUsage(ctx, "default", "test-workspace")
			elapsed := time.Since(start)

			// Fetch should return a degraded response (nil metrics), not an
			// error, well before the metricsServerCallTimeout ceiling.
			Expect(err).NotTo(HaveOccurred())
			Expect(got).NotTo(BeNil())
			Expect(got.Containers["container-slow"].MetricsFromMetricsServer).To(BeNil())
			Expect(elapsed).To(BeNumerically("<=", metricsServerCallTimeout+2*time.Second))

			// The timeout path should have written a negative-TTL entry so
			// a rapid retry does not immediately re-hammer the upstream.
			_, ok := repo.usageCache.Get("default/test-workspace")
			Expect(ok).To(BeTrue())
		})

		It("evicts oldest entries when cache capacity is exceeded", func() {
			ws1 := &kubefloworgv1beta1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"}}
			ws2 := &kubefloworgv1beta1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ws-2", Namespace: "default"}}
			ws3 := &kubefloworgv1beta1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ws-3", Namespace: "default"}}

			pod1 := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default", UID: "uid-pod-1", Labels: map[string]string{modelsCommon.LabelWorkspaceName: "ws-1"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c1"}}},
			}
			pod2 := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: "default", UID: "uid-pod-2", Labels: map[string]string{modelsCommon.LabelWorkspaceName: "ws-2"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c2"}}},
			}
			pod3 := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-3", Namespace: "default", UID: "uid-pod-3", Labels: map[string]string{modelsCommon.LabelWorkspaceName: "ws-3"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c3"}}},
			}

			metrics1 := workspacePodMetrics("pod-1", "c1", nil)
			metrics2 := workspacePodMetrics("pod-2", "c2", nil)
			metrics3 := workspacePodMetrics("pod-3", "c3", nil)

			metricsQueryCount := make(map[string]int)
			var mu sync.Mutex
			cli := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(ws1, ws2, ws3, pod1, pod2, pod3, metrics1, metrics2, metrics3).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, isMetrics := obj.(*metricsv1beta1.PodMetrics); isMetrics {
							mu.Lock()
							metricsQueryCount[key.Name]++
							mu.Unlock()
						}
						return cli.Get(ctx, key, obj, opts...)
					},
				}).
				Build()

			repo := newTestMetricsRepository(cli, true, 2)

			// Query ws-1 (miss → pod-1 metrics).
			_, err := repo.GetWorkspaceResourceUsage(ctx, "default", "ws-1")
			Expect(err).NotTo(HaveOccurred())
			// Query ws-2 (miss → pod-2 metrics).
			_, err = repo.GetWorkspaceResourceUsage(ctx, "default", "ws-2")
			Expect(err).NotTo(HaveOccurred())
			// Query ws-1 (hit, ws-1 becomes MRU).
			_, err = repo.GetWorkspaceResourceUsage(ctx, "default", "ws-1")
			Expect(err).NotTo(HaveOccurred())
			// Query ws-3 (miss; capacity=2 evicts ws-2 which was LRU).
			_, err = repo.GetWorkspaceResourceUsage(ctx, "default", "ws-3")
			Expect(err).NotTo(HaveOccurred())
			// Query ws-2 (miss; was evicted).
			_, err = repo.GetWorkspaceResourceUsage(ctx, "default", "ws-2")
			Expect(err).NotTo(HaveOccurred())

			mu.Lock()
			defer mu.Unlock()
			Expect(metricsQueryCount["pod-1"]).To(Equal(1))
			Expect(metricsQueryCount["pod-2"]).To(Equal(2))
			Expect(metricsQueryCount["pod-3"]).To(Equal(1))
		})
	})
})

var _ = Describe("metricsAPIServed", func() {
	It("reports (true, nil) when the PodMetrics kind resolves", func() {
		mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{metricsv1beta1.SchemeGroupVersion})
		mapper.Add(metricsv1beta1.SchemeGroupVersion.WithKind("PodMetrics"), meta.RESTScopeNamespace)
		c := fake.NewClientBuilder().WithRESTMapper(mapper).Build()

		served, err := metricsAPIServed(c)
		Expect(err).NotTo(HaveOccurred())
		Expect(served).To(BeTrue())
	})

	It("reports (false, nil) when the kind is confirmed absent — steady state, not transient (review finding #3)", func() {
		// A NoMatchError is a *definitive* absent answer and must be
		// cached at the positive TTL, not the short negative TTL —
		// otherwise clusters legitimately not running metrics-server churn
		// RESTMapper every few seconds forever.
		c := fake.NewClientBuilder().WithRESTMapper(meta.NewDefaultRESTMapper(nil)).Build()

		served, err := metricsAPIServed(c)
		Expect(err).NotTo(HaveOccurred())
		Expect(served).To(BeFalse())
	})

	It("reports (false, err) when discovery itself fails — transient (review finding #3)", func() {
		discoveryErr := errors.New("the server is currently unable to handle the request")
		c := fake.NewClientBuilder().WithRESTMapper(failingRESTMapper{err: discoveryErr}).Build()

		served, err := metricsAPIServed(c)
		Expect(err).To(MatchError(discoveryErr))
		Expect(served).To(BeFalse())
	})
})

var _ = Describe("memoize", func() {
	It("calls the probe only once within the positive TTL", func() {
		var calls int32
		available := memoize(time.Minute, time.Millisecond, func() (bool, error) {
			atomic.AddInt32(&calls, 1)
			return true, nil
		})

		Expect(available()).To(BeTrue())
		Expect(available()).To(BeTrue())
		time.Sleep(10 * time.Millisecond)
		Expect(atomic.LoadInt32(&calls)).To(Equal(int32(1)))
	})

	It("caches a definitive negative result at the positive TTL (review finding #3)", func() {
		// (false, nil) means "confirmed absent" — should live for posTTL,
		// not for the short negTTL.
		var calls int32
		available := memoize(time.Minute, time.Millisecond, func() (bool, error) {
			atomic.AddInt32(&calls, 1)
			return false, nil
		})

		Expect(available()).To(BeFalse())
		Expect(available()).To(BeFalse())
		time.Sleep(10 * time.Millisecond)
		Expect(atomic.LoadInt32(&calls)).To(Equal(int32(1)))
	})

	It("re-probes soon when the probe returns an error — treated as transient (review finding #3)", func() {
		var calls int32
		available := memoize(time.Minute, 5*time.Millisecond, func() (bool, error) {
			atomic.AddInt32(&calls, 1)
			return false, errors.New("transient")
		})

		Expect(available()).To(BeFalse())
		Expect(atomic.LoadInt32(&calls)).To(Equal(int32(1)))

		time.Sleep(20 * time.Millisecond)

		// After the negative TTL has passed, the next call triggers a
		// stale-refresh via a background goroutine; give it a moment to
		// complete then confirm the probe re-ran.
		_ = available()
		Eventually(func() int32 { return atomic.LoadInt32(&calls) }).Should(BeNumerically(">=", 2))
	})

	It("picks up a change in underlying state after the positive TTL expires", func() {
		var served atomic.Bool
		available := memoize(5*time.Millisecond, time.Millisecond, func() (bool, error) {
			return served.Load(), nil
		})

		Expect(available()).To(BeFalse())

		served.Store(true)
		time.Sleep(20 * time.Millisecond)

		// First call after TTL returns the previous (stale) value and
		// kicks off a refresh; the next call sees the fresh value.
		_ = available()
		Eventually(available).Should(BeTrue())
	})

	It("coalesces concurrent cold-start callers into a single probe execution (review finding #2)", func() {
		var calls int32
		release := make(chan struct{})
		available := memoize(time.Minute, time.Second, func() (bool, error) {
			atomic.AddInt32(&calls, 1)
			<-release
			return true, nil
		})

		const N = 30
		results := make(chan bool, N)
		for i := 0; i < N; i++ {
			go func() { results <- available() }()
		}

		// All N callers should now be blocked on the singleflight leader.
		Eventually(func() int32 { return atomic.LoadInt32(&calls) }).Should(Equal(int32(1)))
		Consistently(func() int32 { return atomic.LoadInt32(&calls) }, 50*time.Millisecond, 5*time.Millisecond).Should(Equal(int32(1)))

		close(release)
		for i := 0; i < N; i++ {
			Expect(<-results).To(BeTrue())
		}
		Expect(atomic.LoadInt32(&calls)).To(Equal(int32(1)))
	})

	It("serves the previous value immediately (stale-while-revalidate) on TTL expiry — no caller blocks (review finding #2)", func() {
		var calls int32
		probeGate := make(chan struct{}, 8)
		available := memoize(5*time.Millisecond, time.Millisecond, func() (bool, error) {
			atomic.AddInt32(&calls, 1)
			<-probeGate
			return true, nil
		})

		// Warm-up: first call blocks on the cold-start probe.
		probeGate <- struct{}{}
		Expect(available()).To(BeTrue())

		// Wait for the positive TTL to expire.
		time.Sleep(20 * time.Millisecond)

		// Now call again — should return immediately with the previous
		// value, kicking off a background refresh that's still blocked
		// on probeGate. The caller should NOT be blocked.
		done := make(chan bool, 1)
		go func() { done <- available() }()
		Eventually(done).Should(Receive(Equal(true)))

		// Release the background refresh cleanly and confirm at least
		// one refresh probe executed.
		probeGate <- struct{}{}
		Eventually(func() int32 { return atomic.LoadInt32(&calls) }).Should(BeNumerically(">=", 2))
	})
})

type failingRESTMapper struct {
	meta.RESTMapper
	err error
}

func (m failingRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	return nil, m.err
}

// testWorkspaceCR is the Workspace the specs resolve usage for.
// GetWorkspaceResourceUsage confirms the workspace exists before reading
// usage, so it must be present in the fake client.
func testWorkspaceCR() *kubefloworgv1beta1.Workspace {
	return &kubefloworgv1beta1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-workspace",
			Namespace: "default",
		},
	}
}

func newTestMetricsRepository(c client.Client, apiAvailable bool, cacheCapacity int) *MetricsRepository {
	return &MetricsRepository{
		client:       c,
		apiAvailable: func() bool { return apiAvailable },
		usageCache:   cache.NewLRUExpireCache(cacheCapacity),
	}
}

func workspacePod(name, containerName string, requests corev1.ResourceList) *corev1.Pod {
	c := corev1.Container{
		Name: containerName,
	}
	if requests != nil {
		c.Resources = corev1.ResourceRequirements{
			Requests: requests,
		}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
			Labels: map[string]string{
				modelsCommon.LabelWorkspaceName: "test-workspace",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{c},
		},
	}
}

func workspacePodMetrics(name, containerName string, usage corev1.ResourceList) *metricsv1beta1.PodMetrics {
	return &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				modelsCommon.LabelWorkspaceName: "test-workspace",
			},
		},
		Containers: []metricsv1beta1.ContainerMetrics{
			{
				Name:  containerName,
				Usage: usage,
			},
		},
	}
}
