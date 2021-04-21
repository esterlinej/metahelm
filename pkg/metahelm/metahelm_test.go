package metahelm

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	dockerauth "github.com/deislabs/oras/pkg/auth/docker"
	"helm.sh/helm/v3/internal/experimental/registry"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	mtypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

var testCharts = []Chart{
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

func gentestobjs() []runtime.Object {
	objs := []runtime.Object{}
	reps := int32(1)
	iscontroller := true
	rsl := appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{}}
	for _, c := range testCharts {
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
		r.Namespace = DefaultK8sNamespace
		r.Labels = d.Spec.Template.Labels
		d.Labels = d.Spec.Template.Labels
		d.ObjectMeta.UID = mtypes.UID(c.Name() + "-deployment")
		r.ObjectMeta.OwnerReferences = []metav1.OwnerReference{metav1.OwnerReference{UID: d.ObjectMeta.UID, Controller: &iscontroller}}
		d.Name = c.Name()
		d.Namespace = DefaultK8sNamespace
		r.Spec.Template = d.Spec.Template
		objs = append(objs, d)
		rsl.Items = append(rsl.Items, *r)
	}
	return append(objs, &rsl)
}

// Copied from helm.sh/helm/v3/pkg/action/action_test.go
func actionConfigFixture(t *testing.T) *Configuration {
	t.Helper()

	client, err := dockerauth.NewClient()
	if err != nil {
		t.Fatal(err)
	}

	resolver, err := client.Resolver(context.Background(), http.DefaultClient, false)
	if err != nil {
		t.Fatal(err)
	}

	tdir, err := ioutil.TempDir("", "helm-action-test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { os.RemoveAll(tdir) })

	cache, err := registry.NewCache(
		registry.CacheOptDebug(true),
		registry.CacheOptRoot(filepath.Join(tdir, registry.CacheRootDir)),
	)
	if err != nil {
		t.Fatal(err)
	}

	registryClient, err := registry.NewClient(
		registry.ClientOptAuthorizer(&registry.Authorizer{
			Client: client,
		}),
		registry.ClientOptResolver(&registry.Resolver{
			Resolver: resolver,
		}),
		registry.ClientOptCache(cache),
	)
	if err != nil {
		t.Fatal(err)
	}

	return &Configuration{
		Releases:       storage.Init(driver.NewMemory()),
		KubeClient:     &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: ioutil.Discard}},
		Capabilities:   chartutil.DefaultCapabilities,
		RegistryClient: registryClient,
		Log: func(format string, v ...interface{}) {
			t.Helper()
			if *verbose {
				t.Logf(format, v...)
			}
		},
	}
}

func fakeRegistryCache(t *testing.T) *registry.Cache {
	t.Helper()
	tdir, err := ioutil.TempDir("", "helm-action-test")
	if err != nil {
		t.Fatalf("error creating temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tdir) })
	cache, err := registry.NewCache(
		registry.CacheOptDebug(true),
		registry.CacheOptRoot(filepath.Join(tdir, registry.CacheRootDir)),
	)
	if err != nil {
		t.Fatalf("error creating cache: %v", err)
	}
	return cache
}

func fakeRegistryClient(t *testing.T) *registry.Client {
	t.Helper()
	client, err := dockerauth.NewClient()
	if err != nil {
		t.Fatalf("error creating Docker auth client: %v", err)
	}
	resolver, err := client.Resolver(context.Background(), http.DefaultClient, false)
	if err != nil {
		t.Fatalf("error creating Docker auth client resolver: %v", err)
	}
	cache := fakeRegistryCache(t)
	registryClient, err := registry.NewClient(
		registry.ClientOptAuthorizer(&registry.Authorizer{
			Client: client,
		}),
		registry.ClientOptResolver(&registry.Resolver{
			Resolver: resolver,
		}),
		registry.ClientOptCache(cache),
	)
	if err != nil {
		t.Fatalf("error creating registry client: %v", err)
	}
	return registryClient
}

func fakeHelmConfiguration(t *testing.T) *action.Configuration {
	t.Helper()
	releases := storage.Init(nil)
	kubeClient := &kubefake.PrintingKubeClient{Out: ioutil.Discard}
	registryClient := fakeRegistryClient(t)
	return &action.Configuration{
		Releases:       releases,
		KubeClient:     kubeClient,
		RegistryClient: registryClient,
		Capabilities:   chartutil.DefaultCapabilities,
		Log: func(format string, v ...interface{}) {
			t.Helper()
			t.Logf(format, v)
		},
	}
}

func fakeKubernetesClientset(t *testing.T, cfg *action.Configuration) kubernetes.Interface {
	t.Helper()
	clientset, err := cfg.KubernetesClientSet()
	if err != nil {
		t.Fatalf("error getting Kubernetes clientset from Helm action configuration: %v", err)
	}
	return clientset
}

func TestGraphInstall(t *testing.T) {
	cfg := fakeHelmConfiguration(t)
	fkc := fakeKubernetesClientset(t, cfg)
	cfg.KubernetesClientSet()
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

func TestGraphInstallCompletedCallback(t *testing.T) {
	cfg := fakeHelmConfiguration(t)
	fkc := fakeKubernetesClientset(t, cfg)
	cfg.KubernetesClientSet()
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
	cfg := fakeHelmConfiguration(t)
	fkc := fakeKubernetesClientset(t, cfg)
	cfg.KubernetesClientSet()
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
	cfg := fakeHelmConfiguration(t)
	fkc := fakeKubernetesClientset(t, cfg)
	cfg.KubernetesClientSet()
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
	cfg := fakeHelmConfiguration(t)
	fkc := fakeKubernetesClientset(t, cfg)
	cfg.KubernetesClientSet()
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

func TestGraphUpgrade(t *testing.T) {
	cfg := fakeHelmConfiguration(t)
	fkc := fakeKubernetesClientset(t, cfg)
	cfg.KubernetesClientSet()
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	um := ReleaseMap{}
	rels := []*rls.Release{}
	for i, c := range testCharts {
		rn := fmt.Sprintf("release-%v-%v", c.Title, i)
		um[c.Title] = rn
		rels = append(rels, &rls.Release{Name: rn})
	}
	fhc.Rels = rels
	err := m.Upgrade(context.Background(), um, testCharts)
	if err != nil {
		t.Fatalf("error upgrading: %v", err)
	}
}

func TestGraphUpgradeMissingRelease(t *testing.T) {
	cfg := fakeHelmConfiguration(t)
	fkc := fakeKubernetesClientset(t, cfg)
	cfg.KubernetesClientSet()
	m := Manager{
		LogF: t.Logf,
		K8c:  fkc,
		HCfg: cfg,
	}
	ChartWaitPollInterval = 1 * time.Second
	um := ReleaseMap{}
	rels := []*rls.Release{}
	for i, c := range testCharts {
		rn := fmt.Sprintf("release-%v-%v", c.Title, i)
		um[c.Title] = rn
		rels = append(rels, &rls.Release{Name: rn})
	}
	fhc.Rels = rels
	delete(um, testCharts[0].Title)
	err := m.Upgrade(context.Background(), um, testCharts)
	if err == nil {
		t.Fatalf("should have failed")
	}
}

func TestReleaseName(t *testing.T) {
	cases := []struct {
		name, input string
	}{
		{
			"short", "some-release-name",
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
		})
	}
}
