package metahelm

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	mtypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var testCharts = []Chart{
	Chart{
		Title:                      "toplevel",
		Location:                   "testdata/chart",
		WaitUntilDeployment:        "toplevel",
		DeploymentHealthIndication: IgnorePodHealth,
		DependencyList:             []string{"someservice", "anotherthing", "redis"},
	},
	Chart{
		Title:                      "someservice",
		Location:                   "testdata/chart",
		WaitUntilDeployment:        "someservice",
		DeploymentHealthIndication: IgnorePodHealth,
	},
	Chart{
		Title:                      "anotherthing",
		Location:                   "testdata/chart",
		WaitUntilDeployment:        "anotherthing",
		DeploymentHealthIndication: AllPodsHealthy,
		WaitTimeout:                2 * time.Second,
		DependencyList:             []string{"redis"},
	},
	Chart{
		Title:                      "redis",
		Location:                   "testdata/chart",
		DeploymentHealthIndication: IgnorePodHealth,
	},
}

var testChartsLongReleaseNames = []Chart{
	Chart{
		Title:                      "toplevel-application-long-name-to-be-truncated",
		Location:                   "testdata/chart",
		WaitUntilDeployment:        "toplevel-application-long-name-to-be-truncated",
		DeploymentHealthIndication: IgnorePodHealth,
		DependencyList:             []string{"someservice-dependency-long-name-to-be-truncated", "anotherthing-dependency-long-name-to-be-truncated", "redis"},
	},
	Chart{
		Title:                      "someservice-dependency-long-name-to-be-truncated",
		Location:                   "testdata/chart",
		WaitUntilDeployment:        "someservice-dependency-long-name-to-be-truncated",
		DeploymentHealthIndication: IgnorePodHealth,
	},
	Chart{
		Title:                      "anotherthing-dependency-long-name-to-be-truncated",
		Location:                   "testdata/chart",
		WaitUntilDeployment:        "anotherthing-dependency-long-name-to-be-truncated",
		DeploymentHealthIndication: AllPodsHealthy,
		WaitTimeout:                2 * time.Second,
		DependencyList:             []string{"redis"},
	},
	Chart{
		Title:                      "redis",
		Location:                   "testdata/chart",
		DeploymentHealthIndication: IgnorePodHealth,
	},
}

var testChartsCapitalized = []Chart{
	Chart{
		Title:                      "Toplevel",
		Location:                   "Testdata/chart",
		WaitUntilDeployment:        "Toplevel",
		DeploymentHealthIndication: IgnorePodHealth,
		DependencyList:             []string{"SomeService", "anotherThing", "REDIS"},
	},
	Chart{
		Title:                      "SomeService",
		Location:                   "Testdata/chart",
		WaitUntilDeployment:        "SomeService",
		DeploymentHealthIndication: IgnorePodHealth,
	},
	Chart{
		Title:                      "anotherThing",
		Location:                   "Testdata/chart",
		WaitUntilDeployment:        "anotherThing",
		DeploymentHealthIndication: AllPodsHealthy,
		WaitTimeout:                2 * time.Second,
		DependencyList:             []string{"REDIS"},
	},
	Chart{
		Title:                      "REDIS",
		Location:                   "Testdata/chart",
		DeploymentHealthIndication: IgnorePodHealth,
	},
}

func gentestobjs(namespace string, charts []Chart) []runtime.Object {
	if namespace == "" {
		namespace = DefaultK8sNamespace
	}
	objs := []runtime.Object{}
	reps := int32(1)
	iscontroller := true
	rsl := appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{}}
	for _, c := range charts {
		r := &appsv1.ReplicaSet{}
		d := &appsv1.Deployment{}
		d.Spec.Replicas = &reps
		d.Spec.Template.Labels = map[string]string{"app": c.Name()}
		d.Spec.Template.Spec.NodeSelector = map[string]string{}
		d.Spec.Template.Name = c.Name()
		d.Spec.Selector = &metav1.LabelSelector{}
		d.Spec.Selector.MatchLabels = map[string]string{"app": c.Name()}
		r.Spec.Selector = d.Spec.Selector
		r.Spec.Replicas = &reps
		r.Status.ReadyReplicas = 1
		r.Name = "replicaset-" + c.Name()
		r.Namespace = namespace
		d.Spec.Replicas = &reps
		d.Status.ReadyReplicas = 1
		r.Labels = d.Spec.Template.Labels
		d.Labels = d.Spec.Template.Labels
		d.ObjectMeta.UID = mtypes.UID(c.Name() + "-deployment")
		r.ObjectMeta.OwnerReferences = []metav1.OwnerReference{metav1.OwnerReference{UID: d.ObjectMeta.UID, Controller: &iscontroller}}
		d.Name = c.Name()
		d.Namespace = namespace
		r.Spec.Template = d.Spec.Template
		objs = append(objs, d)
		rsl.Items = append(rsl.Items, *r)
	}
	return append(objs, &rsl)
}

// testKubeClient is a stub helm v3 internal kube client for testing purposes
type testKubeClient struct {
}

var _ kube.Interface = &testKubeClient{}

func (tkc *testKubeClient) Create(resources kube.ResourceList) (*kube.Result, error) {
	return &kube.Result{}, nil
}

func (tkc *testKubeClient) Wait(resources kube.ResourceList, timeout time.Duration) error {
	return nil
}

func (tkc *testKubeClient) WaitWithJobs(resources kube.ResourceList, timeout time.Duration) error {
	return nil
}

func (tkc *testKubeClient) Delete(resources kube.ResourceList) (*kube.Result, []error) {
	return &kube.Result{}, nil
}

func (tkc *testKubeClient) WatchUntilReady(resources kube.ResourceList, timeout time.Duration) error {
	return nil
}

func (tkc *testKubeClient) Update(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	return &kube.Result{}, nil
}

func (tkc *testKubeClient) Build(reader io.Reader, validate bool) (kube.ResourceList, error) {
	return kube.ResourceList{}, nil
}

func (tkc *testKubeClient) WaitAndGetCompletedPodPhase(name string, timeout time.Duration) (corev1.PodPhase, error) {
	return "", nil
}

func (tkc *testKubeClient) IsReachable() error {
	return nil
}

var _ action.RESTClientGetter = &testKubeClient{}

func (tkc *testKubeClient) ToRESTConfig() (*rest.Config, error) {
	return nil, nil
}

func (tkc *testKubeClient) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return nil, nil
}

func (tkc *testKubeClient) ToRESTMapper() (meta.RESTMapper, error) {
	return nil, nil
}

func (tkc *testKubeClient) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return nil
}

func fakeHelmConfiguration(t *testing.T) *action.Configuration {
	t.Helper()
	ac := &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   &testKubeClient{},
		Capabilities: chartutil.DefaultCapabilities,
		Log: func(format string, v ...interface{}) {
			t.Helper()
			t.Logf(format, v)
		},
	}
	return ac
}

func fakeKubernetesClientset(t *testing.T, namespace string, charts []Chart) kubernetes.Interface {
	t.Helper()
	kubeClient := k8sfake.NewSimpleClientset(gentestobjs(namespace, charts)...)
	return kubeClient
}

func TestGraphInstall(t *testing.T) {
	fkc := fakeKubernetesClientset(t, DefaultK8sNamespace, testCharts)
	cfg := fakeHelmConfiguration(t)
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	rm, err := m.Install(context.Background(), testCharts)
	if err != nil {
		t.Fatalf("error installing: %v", err)
	}
	t.Logf("rm: %v\n", rm)
}

func TestGraphInstallWithReleaseNamePrefix(t *testing.T) {
	prefix := "metahelm-test-prefix-"
	fkc := fakeKubernetesClientset(t, DefaultK8sNamespace, testCharts)
	cfg := fakeHelmConfiguration(t)
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	rm, err := m.Install(context.Background(), testCharts, WithReleaseNamePrefix(prefix))
	if err != nil {
		t.Fatalf("error installing: %v", err)
	}
	t.Logf("rm: %v\n", rm)
}

func TestGraphInstallLongReleaseName(t *testing.T) {
	prefix := "metahelm-test-prefix-"
	fkc := fakeKubernetesClientset(t, DefaultK8sNamespace, testChartsLongReleaseNames)
	cfg := fakeHelmConfiguration(t)
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	rm, err := m.Install(context.Background(), testChartsLongReleaseNames, WithReleaseNamePrefix(prefix))
	if err != nil {
		t.Fatalf("error installing: %v", err)
	}
	t.Logf("rm: %v\n", rm)
}

func TestGraphInstallCapitalized(t *testing.T) {
	prefix := "metahelm-test-prefix-"
	fkc := fakeKubernetesClientset(t, DefaultK8sNamespace, testChartsCapitalized)
	cfg := fakeHelmConfiguration(t)
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	rm, err := m.Install(context.Background(), testChartsCapitalized, WithReleaseNamePrefix(prefix))
	if err != nil {
		t.Fatalf("error installing: %v", err)
	}
	t.Logf("rm: %v\n", rm)
}

func TestGraphInstallWithK8sNamespace(t *testing.T) {
	ns := "foo"
	fkc := fakeKubernetesClientset(t, ns, testCharts)
	cfg := fakeHelmConfiguration(t)
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	rm, err := m.Install(context.Background(), testCharts, WithK8sNamespace(ns))
	if err != nil {
		t.Fatalf("error installing: %v", err)
	}
	t.Logf("rm: %v\n", rm)
	for _, v := range rm {
		r, _ := m.HCfg.Releases.Deployed(v)
		if r.Namespace != ns {
			t.Fatalf("error incorrect namespace; expected: (%v), got: (%v)", ns, v)
		}
	}
}

func TestGraphInstallCompletedCallback(t *testing.T) {
	fkc := fakeKubernetesClientset(t, DefaultK8sNamespace, testCharts)
	cfg := fakeHelmConfiguration(t)
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	var called int64
	_, err := m.Install(context.Background(), testCharts, WithCompletedCallback(func(c Chart, err error) {
		atomic.AddInt64(&called, 1)
	}))
	if err != nil {
		t.Fatalf("error installing: %v", err)
	}
	if called != 4 {
		t.Fatalf("unexpected called result (wanted 4): %v", called)
	}
}

func TestGraphInstallWaitCallback(t *testing.T) {
	fkc := fakeKubernetesClientset(t, DefaultK8sNamespace, testCharts)
	cfg := fakeHelmConfiguration(t)
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	var i int
	cb := func(c Chart) InstallCallbackAction {
		if c.Name() != testCharts[1].Name() {
			return Continue
		}
		if i >= 2 {
			return Continue
		}
		i++
		return Wait
	}
	retryDelay = 10 * time.Millisecond
	_, err := m.Install(context.Background(), testCharts, WithInstallCallback(cb))
	if err != nil {
		t.Fatalf("error installing: %v", err)
	}
	if i < 2 {
		t.Fatalf("bad callback count: %v", i)
	}
}

func TestGraphInstallAbortCallback(t *testing.T) {
	fkc := fakeKubernetesClientset(t, DefaultK8sNamespace, testCharts)
	cfg := fakeHelmConfiguration(t)
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	var i int
	cb := func(c Chart) InstallCallbackAction {
		i++
		if c.Name() == testCharts[3].Name() {
			return Abort
		}
		return Continue
	}
	retryDelay = 10 * time.Millisecond
	_, err := m.Install(context.Background(), testCharts, WithInstallCallback(cb))
	if err == nil {
		t.Fatalf("should have failed")
	}
	if i != 1 {
		t.Fatalf("bad callback count: %v", i)
	}
}

func TestGraphInstallTimeout(t *testing.T) {
	fkc := fakeKubernetesClientset(t, DefaultK8sNamespace, testCharts)
	cfg := fakeHelmConfiguration(t)
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	cb := func(c Chart) InstallCallbackAction {
		if c.Name() != testCharts[1].Name() {
			return Continue
		}
		return Wait
	}
	retryDelay = 10 * time.Millisecond
	_, err := m.Install(context.Background(), testCharts, WithInstallCallback(cb), WithTimeout(100*time.Millisecond))
	if err == nil {
		t.Fatalf("should have returned an error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("error: %v", err)
}

func TestValidateCharts(t *testing.T) {
	charts := []Chart{
		Chart{
			Title:                      "toplevel",
			Location:                   "/foo",
			WaitUntilDeployment:        "toplevel",
			DeploymentHealthIndication: IgnorePodHealth,
			DependencyList:             []string{"someservice", "anotherthing", "redis"},
		},
		Chart{
			Title:                      "someservice",
			Location:                   "/foo",
			WaitUntilDeployment:        "someservice",
			DeploymentHealthIndication: IgnorePodHealth,
		},
		Chart{
			Title:                      "anotherthing",
			Location:                   "/foo",
			WaitUntilDeployment:        "anotherthing",
			DeploymentHealthIndication: AllPodsHealthy,
			WaitTimeout:                2 * time.Second,
			DependencyList:             []string{"redis"},
		},
		Chart{
			Title:                      "redis",
			Location:                   "/foo",
			DeploymentHealthIndication: IgnorePodHealth,
		},
	}
	if err := ValidateCharts(charts); err != nil {
		t.Fatalf("should have succeeded: %v", err)
	}
	charts[3].DependencyList = []string{"anotherthing"}
	if err := ValidateCharts(charts); err == nil {
		t.Fatalf("should have failed with dependency cycle")
	}
	charts[3].DependencyList = nil
	charts[3].Title = ""
	if err := ValidateCharts(charts); err == nil {
		t.Fatalf("should have failed with empty title")
	}
	charts[3].Title = "redis"
	charts[3].Location = ""
	if err := ValidateCharts(charts); err == nil {
		t.Fatalf("should have failed with empty location")
	}
	charts[3].Location = "/foo"
	charts[3].DeploymentHealthIndication = 9999
	if err := ValidateCharts(charts); err == nil {
		t.Fatalf("should have failed with invalid DeploymentHealthIndication")
	}
	charts[3].DeploymentHealthIndication = IgnorePodHealth
	charts[3].DependencyList = []string{"doesntexist"}
	if err := ValidateCharts(charts); err == nil {
		t.Fatalf("should have failed with unknown dependency")
	}
}

func TestGraphInstallAndUpgrade(t *testing.T) {
	ns := "foo"
	prefix := "metahelm-test-prefix-"
	fkc := fakeKubernetesClientset(t, ns, testCharts)
	cfg := fakeHelmConfiguration(t)
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	um, err := m.Install(context.Background(), testCharts, WithK8sNamespace(ns), WithReleaseNamePrefix(prefix))
	if err != nil {
		t.Fatalf("error upgrading: %v", err)
	}
	lr, err := m.HCfg.Releases.ListReleases()
	if err != nil {
		t.Fatalf("error listing releases: %v", err)
	}
	count := 0
	for _, r := range lr {
		if r.Namespace != ns {
			t.Fatalf("error incorrect namespace on install; expected: %v, got: %v", ns, r.Namespace)
		}
		if r.Version != 1 {
			t.Fatalf("error incorrect version on install; expected: v1, got: v%v", r.Version)
		}
		t.Logf("installed: %v, v%v", r.Name, r.Version)
		count += 1
	}
	if count != len(um) {
		t.Fatalf("error incorrect number of releases upgraded; expected: %v, got: %v", len(um), count)
	}
	err = m.Upgrade(context.Background(), um, testCharts, WithK8sNamespace(ns), WithReleaseNamePrefix(prefix))
	if err != nil {
		t.Fatalf("error upgrading: %v", err)
	}
	lr, err = m.HCfg.Releases.ListReleases()
	if err != nil {
		t.Fatalf("error listing releases: %v", err)
	}
	count = 0
	for _, r := range lr {
		if r.Version == 2 {
			if r.Namespace != ns {
				t.Fatalf("error incorrect namespace on upgrade; expected: %v, got: %v", ns, r.Namespace)
			}
			t.Logf("upgraded: %v, v%v", r.Name, r.Version)
			count += 1
		}
	}
	if count != len(um) {
		t.Fatalf("error incorrect number of releases upgraded; expected: %v, got: %v", len(um), count)
	}
}

func TestGraphInstallAndUpgradeMissingRelease(t *testing.T) {
	ns := "foo"
	prefix := "metahelm-test-prefix-"
	fkc := fakeKubernetesClientset(t, ns, testCharts)
	cfg := fakeHelmConfiguration(t)
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	um, err := m.Install(context.Background(), testCharts, WithK8sNamespace(ns), WithReleaseNamePrefix(prefix))
	if err != nil {
		t.Fatalf("error upgrading: %v", err)
	}
	lr, err := m.HCfg.Releases.ListReleases()
	if err != nil {
		t.Fatalf("error listing releases: %v", err)
	}
	for _, r := range lr {
		found := false
		for _, v := range um {
			if v == r.Name {
				found = true
			}
		}
		if !found {
			t.Fatalf("error release not found on upgrade: %v", r.Name)
		}
		if r.Namespace != ns {
			t.Fatalf("error incorrect namespace on upgrade; expected: %v, got: %v", ns, r.Namespace)
		}
		if r.Version != 1 {
			t.Fatalf("error incorrect version on install; expected: v1, got: v%v", r.Version)
		}
		t.Logf("installed: %v, v%v", r.Name, r.Version)
	}
	delete(um, testCharts[0].Title)
	err = m.Upgrade(context.Background(), um, testCharts, WithK8sNamespace(ns), WithReleaseNamePrefix(prefix))
	if err == nil {
		t.Fatalf("should have failed")
	}
	t.Logf("error: %v", err)
}

func TestReleaseName(t *testing.T) {
	cases := []struct {
		name, input string
	}{
		{
			"short", "some-release-name",
		},
		{
			"capitalized", "Capitalized-Release-Name",
		},
		{
			"long", "this-is-an-exceedingly-long-release-name-that-would-fail-installation",
		},
		{
			"short unicode", "⌘日本語-name",
		},
		{
			"long unicode", "⌘日本語-⌘日本語-⌘日本語-⌘日本語-⌘日本語-⌘日本語-⌘日本語-⌘日本語-⌘日本語-⌘日本語-long-name",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := ReleaseName(c.input)
			if i := utf8.RuneCountInString(out); i > 53 {
				t.Fatalf("length exceeds max of 53: %v", i)
			}
			if out == "" {
				t.Fatalf("blank output")
			}
			for _, c := range out {
				if unicode.IsLetter(c) && unicode.IsUpper(c) {
					t.Fatalf("capitalization not sanitized")
				}
			}
		})
	}
}
