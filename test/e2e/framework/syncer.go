/*
Copyright 2022 The KCP Authors.

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

package framework

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kcpdynamic "github.com/kcp-dev/client-go/dynamic"
	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	kubernetesclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"

	apiresourcev1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apiresource/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1"
	conditionsv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/third_party/conditions/apis/conditions/v1alpha1"
	"github.com/kcp-dev/kcp/pkg/apis/third_party/conditions/util/conditions"
	workloadv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"
	kcpclientset "github.com/kcp-dev/kcp/pkg/client/clientset/versioned/cluster"
	kcpinformers "github.com/kcp-dev/kcp/pkg/client/informers/externalversions"
	workloadcliplugin "github.com/kcp-dev/kcp/pkg/cliplugins/workload/plugin"
	"github.com/kcp-dev/kcp/pkg/syncer"
	"github.com/kcp-dev/kcp/pkg/syncer/shared"
)

type SyncerOption func(t *testing.T, fs *syncerFixture)

func NewSyncerFixture(t *testing.T, server RunningServer, path logicalcluster.Path, opts ...SyncerOption) *syncerFixture {
	t.Helper()

	if !sets.NewString(TestConfig.Suites()...).HasAny("transparent-multi-cluster", "transparent-multi-cluster:requires-kind") {
		t.Fatalf("invalid to use a syncer fixture when only the following suites were requested: %v", TestConfig.Suites())
	}
	sf := &syncerFixture{
		upstreamServer: server,
		syncTargetPath: path,
		syncTargetName: "psyncer-01",
	}
	for _, opt := range opts {
		opt(t, sf)
	}
	return sf
}

// syncerFixture configures a syncer fixture. Its `Start` method does the work of starting a syncer.
type syncerFixture struct {
	upstreamServer RunningServer

	syncedUserClusterNames []logicalcluster.Name

	syncTargetPath logicalcluster.Path
	syncTargetName string

	extraResourcesToSync []string
	apiExports           []string
	prepareDownstream    func(config *rest.Config, isFakePCluster bool)
}

func WithSyncTargetName(name string) SyncerOption {
	return func(t *testing.T, sf *syncerFixture) {
		t.Helper()
		sf.syncTargetName = name
	}
}

func WithSyncedUserWorkspaces(syncedUserWorkspaces ...*tenancyv1alpha1.Workspace) SyncerOption {
	return func(t *testing.T, sf *syncerFixture) {
		t.Helper()
		for _, ws := range syncedUserWorkspaces {
			sf.syncedUserClusterNames = append(sf.syncedUserClusterNames, logicalcluster.Name(ws.Spec.Cluster))
		}
	}
}

func WithExtraResources(resources ...string) SyncerOption {
	return func(t *testing.T, sf *syncerFixture) {
		t.Helper()
		sf.extraResourcesToSync = append(sf.extraResourcesToSync, resources...)
	}
}

func WithAPIExports(exports ...string) SyncerOption {
	return func(t *testing.T, sf *syncerFixture) {
		t.Helper()
		sf.apiExports = append(sf.apiExports, exports...)
	}
}

func WithDownstreamPreparation(prepare func(config *rest.Config, isFakePCluster bool)) SyncerOption {
	return func(t *testing.T, sf *syncerFixture) {
		t.Helper()
		sf.prepareDownstream = prepare
	}
}

// CreateAndStart creates SyncTarget resource, applies it in the physical cluster,
// and then starts a new syncer against the given upstream kcp workspace.
// Whether the syncer runs in-process or deployed on a pcluster will depend
// whether --pcluster-kubeconfig and --syncer-image are supplied to the test invocation.
func (sf *syncerFixture) CreateAndStart(t *testing.T) *StartedSyncerFixture {
	t.Helper()

	useDeployedSyncer := len(TestConfig.PClusterKubeconfig()) > 0

	artifactDir, _, err := ScratchDirs(t)
	if err != nil {
		t.Errorf("failed to create temp dir for syncer artifacts: %v", err)
	}

	downstreamConfig, downstreamKubeconfigPath, downstreamKubeClient, syncerConfig, syncerID := sf.createAndApplySyncTarget(t, useDeployedSyncer, artifactDir)

	ctx, cancelFunc := context.WithCancel(context.Background())
	t.Cleanup(cancelFunc)

	sf.startSyncer(ctx, t, useDeployedSyncer, downstreamKubeClient, syncerConfig, syncerID, artifactDir, downstreamKubeconfigPath)

	startedSyncer := &StartedSyncerFixture{
		sf.buildAppliedSyncerFixture(ctx, t, downstreamConfig, downstreamKubeClient, syncerConfig, syncerID),
	}

	// The sync target becoming ready indicates the syncer is healthy and has
	// successfully sent a heartbeat to kcp.
	startedSyncer.WaitForClusterReady(ctx, t)

	return startedSyncer
}

func (sf *syncerFixture) createAndApplySyncTarget(t *testing.T, useDeployedSyncer bool, artifactDir string) (downstreamConfig *rest.Config, downstreamKubeconfigPath string, downstreamKubeClient kubernetesclient.Interface, syncerConfig *syncer.SyncerConfig, syncerID string) {
	// Write the upstream logical cluster config to disk for the workspace plugin
	upstreamRawConfig, err := sf.upstreamServer.RawConfig()
	require.NoError(t, err)
	_, kubeconfigPath := WriteLogicalClusterConfig(t, upstreamRawConfig, "base", sf.syncTargetPath)

	syncerImage := TestConfig.SyncerImage()
	if useDeployedSyncer {
		require.NotZero(t, len(syncerImage), "--syncer-image must be specified if testing with a deployed syncer")
	} else {
		// The image needs to be a non-empty string for the plugin command but the value doesn't matter if not deploying a syncer.
		syncerImage = "not-a-valid-image"
	}

	// Run the plugin command to enable the syncer and collect the resulting yaml
	t.Logf("Configuring workspace %s for syncing", sf.syncTargetPath)
	pluginArgs := []string{
		"workload",
		"sync",
		sf.syncTargetName,
		"--syncer-image=" + syncerImage,
		"--output-file=-",
		"--qps=-1",
		"--feature-gates=" + fmt.Sprintf("%s", utilfeature.DefaultFeatureGate),
		"--api-import-poll-interval=5s",
		"--downstream-namespace-clean-delay=2s",
	}
	for _, resource := range sf.extraResourcesToSync {
		pluginArgs = append(pluginArgs, "--resources="+resource)
	}
	for _, export := range sf.apiExports {
		pluginArgs = append(pluginArgs, "--apiexports="+export)
	}
	syncerYAML := RunKcpCliPlugin(t, kubeconfigPath, pluginArgs)

	if useDeployedSyncer {
		// The syncer will target the pcluster identified by `--pcluster-kubeconfig`.
		downstreamKubeconfigPath = TestConfig.PClusterKubeconfig()
		fs, err := os.Stat(downstreamKubeconfigPath)
		require.NoError(t, err)
		require.NotZero(t, fs.Size(), "%s points to an empty file", downstreamKubeconfigPath)
		rawConfig, err := clientcmd.LoadFromFile(downstreamKubeconfigPath)
		require.NoError(t, err, "failed to load pcluster kubeconfig")
		config := clientcmd.NewNonInteractiveClientConfig(*rawConfig, rawConfig.CurrentContext, nil, nil)
		downstreamConfig, err = config.ClientConfig()
		require.NoError(t, err)
	} else {
		// The syncer will target a logical cluster that is a child of the current workspace. A
		// logical server provides as a lightweight approximation of a pcluster for tests that
		// don't need to validate running workloads or interaction with kube controllers.
		downstreamServer := NewFakeWorkloadServer(t, sf.upstreamServer, sf.syncTargetPath, sf.syncTargetName)
		downstreamConfig = downstreamServer.BaseConfig(t)
		downstreamKubeconfigPath = downstreamServer.KubeconfigPath()
	}

	if sf.prepareDownstream != nil {
		// Attempt crd installation to ensure the downstream server has an api surface
		// compatible with the test.
		sf.prepareDownstream(downstreamConfig, !useDeployedSyncer)
	}

	// Apply the yaml output from the plugin to the downstream server
	KubectlApply(t, downstreamKubeconfigPath, syncerYAML)

	// collect both in deployed and in-process mode
	t.Cleanup(func() {
		ctx, cancelFn := context.WithDeadline(context.Background(), time.Now().Add(wait.ForeverTestTimeout))
		defer cancelFn()

		t.Logf("Collecting imported resource info: %s", artifactDir)
		upstreamCfg := sf.upstreamServer.BaseConfig(t)

		gather := func(client dynamic.Interface, gvr schema.GroupVersionResource) {
			resourceClient := client.Resource(gvr)

			list, err := resourceClient.List(ctx, metav1.ListOptions{})
			if err != nil {
				// Don't fail the test
				t.Logf("Error gathering %s: %v", gvr, err)
				return
			}

			for i := range list.Items {
				item := list.Items[i]
				sf.upstreamServer.Artifact(t, func() (runtime.Object, error) {
					return &item, nil
				})
			}
		}

		upstreamClusterDynamic, err := kcpdynamic.NewForConfig(upstreamCfg)
		require.NoError(t, err, "error creating upstream dynamic client")

		downstreamDynamic, err := dynamic.NewForConfig(downstreamConfig)
		require.NoError(t, err, "error creating downstream dynamic client")

		kcpClusterClient, err := kcpclientset.NewForConfig(upstreamCfg)
		require.NoError(t, err, "error creating upstream kcp client")

		gather(upstreamClusterDynamic.Cluster(sf.syncTargetPath), apiresourcev1alpha1.SchemeGroupVersion.WithResource("apiresourceimports"))
		gather(upstreamClusterDynamic.Cluster(sf.syncTargetPath), apiresourcev1alpha1.SchemeGroupVersion.WithResource("negotiatedapiresources"))
		gather(upstreamClusterDynamic.Cluster(sf.syncTargetPath), corev1.SchemeGroupVersion.WithResource("namespaces"))
		gather(downstreamDynamic, corev1.SchemeGroupVersion.WithResource("namespaces"))

		syncTarget, err := kcpClusterClient.Cluster(sf.syncTargetPath).WorkloadV1alpha1().SyncTargets().Get(ctx, sf.syncTargetName, metav1.GetOptions{})
		require.NoError(t, err)

		for _, resource := range syncTarget.Status.SyncedResources {
			for _, version := range resource.Versions {
				gvr := schema.GroupVersionResource{
					Group:    resource.Group,
					Resource: resource.Resource,
					Version:  version,
				}
				for _, syncedUserClusterName := range sf.syncedUserClusterNames {
					gather(upstreamClusterDynamic.Cluster(syncedUserClusterName.Path()), gvr)
				}
				gather(downstreamDynamic, gvr)
			}
		}
	})

	// Extract the configuration for an in-process syncer from the resources that were
	// applied to the downstream server. This maximizes the parity between the
	// configuration of a deployed and in-process syncer.
	for _, doc := range strings.Split(string(syncerYAML), "\n---\n") {
		var manifest struct {
			metav1.ObjectMeta `json:"metadata"`
		}
		err := yaml.Unmarshal([]byte(doc), &manifest)
		require.NoError(t, err)
		if manifest.Namespace != "" {
			syncerID = manifest.Namespace
			break
		}
	}
	require.NotEmpty(t, syncerID, "failed to extract syncer namespace from yaml produced by plugin:\n%s", string(syncerYAML))

	syncerConfig = syncerConfigFromCluster(t, downstreamConfig, syncerID, syncerID)

	downstreamKubeClient, err = kubernetesclient.NewForConfig(downstreamConfig)
	require.NoError(t, err)

	return
}

func (sf *syncerFixture) startSyncer(ctx context.Context, t *testing.T, useDeployedSyncer bool, downstreamKubeClient kubernetesclient.Interface, syncerConfig *syncer.SyncerConfig, syncerID, artifactDir, downstreamKubeconfigPath string) {
	if useDeployedSyncer {
		t.Cleanup(func() {
			ctx, cancelFn := context.WithDeadline(context.Background(), time.Now().Add(wait.ForeverTestTimeout))
			defer cancelFn()

			// collect syncer logs
			t.Logf("Collecting syncer pod logs")
			func() {
				t.Logf("Listing downstream pods in namespace %s", syncerID)
				pods, err := downstreamKubeClient.CoreV1().Pods(syncerID).List(ctx, metav1.ListOptions{})
				if err != nil {
					t.Logf("failed to list pods in %s: %v", syncerID, err)
					return
				}

				for _, pod := range pods.Items {
					// Check if the POD is ready before trying to get the logs, ignore if not to avoid the test failing.
					if pod.Status.Phase != corev1.PodRunning {
						t.Logf("Pod %s is not running", pod.Name)
						continue
					}
					artifactPath := filepath.Join(artifactDir, fmt.Sprintf("syncer-%s-%s.log", syncerID, pod.Name))

					// if the log is stopped or has crashed we will try to get --previous logs.
					extraArg := ""
					if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
						extraArg = "--previous"
					}

					t.Logf("Collecting downstream logs for pod %s/%s: %s", syncerID, pod.Name, artifactPath)
					logs := Kubectl(t, downstreamKubeconfigPath, "-n", syncerID, "logs", pod.Name, extraArg)

					err = os.WriteFile(artifactPath, logs, 0644)
					if err != nil {
						t.Logf("failed to write logs for pod %s in %s to %s: %v", pod.Name, syncerID, artifactPath, err)
						continue // not fatal
					}
				}
			}()

			if preserveTestResources() {
				return
			}

			t.Logf("Deleting syncer resources for sync target %s|%s", syncerConfig.SyncTargetPath, syncerConfig.SyncTargetName)
			err := downstreamKubeClient.CoreV1().Namespaces().Delete(ctx, syncerID, metav1.DeleteOptions{})
			if err != nil {
				t.Errorf("failed to delete Namespace %q: %v", syncerID, err)
			}
			err = downstreamKubeClient.RbacV1().ClusterRoleBindings().Delete(ctx, syncerID, metav1.DeleteOptions{})
			if err != nil {
				t.Errorf("failed to delete ClusterRoleBinding %q: %v", syncerID, err)
			}
			err = downstreamKubeClient.RbacV1().ClusterRoles().Delete(ctx, syncerID, metav1.DeleteOptions{})
			if err != nil {
				t.Errorf("failed to delete ClusterRole %q: %v", syncerID, err)
			}

			t.Logf("Deleting synced resources for sync target %s|%s", syncerConfig.SyncTargetPath, syncerConfig.SyncTargetName)
			namespaces, err := downstreamKubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
			if err != nil {
				t.Errorf("failed to list namespaces: %v", err)
			}
			for _, ns := range namespaces.Items {
				locator, exists, err := shared.LocatorFromAnnotations(ns.Annotations)
				require.NoError(t, err, "failed to extract locator from namespace %s", ns.Name)
				if !exists {
					continue // Not a kcp-synced namespace
				}
				found := false
				for _, syncedUserWorkspace := range sf.syncedUserClusterNames {
					if locator.ClusterName == syncedUserWorkspace {
						found = true
						break
					}
				}
				if !found {
					continue // Not a namespace synced by this Syncer
				}
				if locator.SyncTarget.ClusterName != syncerConfig.SyncTargetPath.String() ||
					locator.SyncTarget.Name != syncerConfig.SyncTargetName {
					continue // Not a namespace synced by this syncer
				}
				if err = downstreamKubeClient.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{}); err != nil {
					t.Logf("failed to delete Namespace %q: %v", ns.Name, err)
				}
			}
		})
	} else {
		// Start an in-process syncer
		syncerConfig.DNSImage = "TODO"
		err := syncer.StartSyncer(ctx, syncerConfig, 2, 5*time.Second, syncerID)
		require.NoError(t, err, "syncer failed to start")

		_, err = downstreamKubeClient.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: "syncer-rbac-fix",
			},
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"*"},
					APIGroups: []string{rbacv1.SchemeGroupVersion.Group},
					Resources: []string{"roles", "rolebindings"},
				},
			},
		}, metav1.CreateOptions{})
		if !apierrors.IsNotFound(err) {
			require.NoError(t, err)
		} else {
			t.Log("Fix ClusterRoleBinding already added")
		}

		_, err = downstreamKubeClient.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "syncer-rbac-fix-" + syncerID,
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.SchemeGroupVersion.Group,
				Kind:     "ClusterRole",
				Name:     "syncer-rbac-fix",
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      syncerID,
					Namespace: syncerID,
				},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		for _, syncedUserWorkspace := range sf.syncedUserClusterNames {
			dnsID := shared.GetDNSID(syncedUserWorkspace, types.UID(syncerConfig.SyncTargetUID), syncerConfig.SyncTargetName)
			_, err := downstreamKubeClient.CoreV1().Endpoints(syncerID).Create(ctx, endpoints(dnsID, syncerID), metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				t.Logf("Failed creating the fake Syncer Endpoint since it already exists - ignoring: %v", err)
			} else {
				require.NoError(t, err)
			}

			// The DNS service may or may not have been created by the spec controller. In any cases, we want to make sure
			// the service ClusterIP is set
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				svc, err := downstreamKubeClient.CoreV1().Services(syncerID).Get(ctx, dnsID, metav1.GetOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					return err
				}
				if apierrors.IsNotFound(err) {
					_, err = downstreamKubeClient.CoreV1().Services(syncerID).Create(ctx, service(dnsID, syncerID), metav1.CreateOptions{})
					if err == nil {
						return nil
					}
					if !apierrors.IsAlreadyExists(err) {
						return err
					}
					svc, err = downstreamKubeClient.CoreV1().Services(syncerID).Get(ctx, dnsID, metav1.GetOptions{})
					if err != nil {
						return err
					}
				}

				svc.Spec.ClusterIP = "8.8.8.8"
				_, err = downstreamKubeClient.CoreV1().Services(syncerID).Update(ctx, svc, metav1.UpdateOptions{})
				return err
			})
			require.NoError(t, err)
		}
	}
}

func (sf *syncerFixture) buildAppliedSyncerFixture(ctx context.Context, t *testing.T, downstreamConfig *rest.Config, downstreamKubeClient kubernetesclient.Interface, syncerConfig *syncer.SyncerConfig, syncerID string) *appliedSyncerFixture {
	rawConfig, err := sf.upstreamServer.RawConfig()
	require.NoError(t, err)

	kcpClusterClient, err := kcpclientset.NewForConfig(syncerConfig.UpstreamConfig)
	require.NoError(t, err)
	var virtualWorkspaceURL string
	var syncTargetClusterName logicalcluster.Name
	Eventually(t, func() (success bool, reason string) {
		syncTarget, err := kcpClusterClient.Cluster(syncerConfig.SyncTargetPath).WorkloadV1alpha1().SyncTargets().Get(ctx, syncerConfig.SyncTargetName, metav1.GetOptions{})
		require.NoError(t, err)
		if len(syncTarget.Status.VirtualWorkspaces) != 1 {
			return false, ""
		}
		virtualWorkspaceURL = syncTarget.Status.VirtualWorkspaces[0].SyncerURL
		syncTargetClusterName = logicalcluster.From(syncTarget)
		return true, "Virtual workspace URL is available"
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "Syncer Virtual Workspace URL not available")

	virtualWorkspaceRawConfig := rawConfig.DeepCopy()
	virtualWorkspaceRawConfig.Clusters["syncer"] = rawConfig.Clusters["base"].DeepCopy()
	virtualWorkspaceRawConfig.Clusters["syncer"].Server = virtualWorkspaceURL
	virtualWorkspaceRawConfig.Contexts["syncer"] = rawConfig.Contexts["base"].DeepCopy()
	virtualWorkspaceRawConfig.Contexts["syncer"].Cluster = "syncer"
	virtualWorkspaceRawConfig.Clusters["upsyncer"] = rawConfig.Clusters["base"].DeepCopy()
	virtualWorkspaceRawConfig.Clusters["upsyncer"].Server = strings.Replace(virtualWorkspaceURL, "/services/syncer/", "/services/upsyncer/", 1)
	virtualWorkspaceRawConfig.Contexts["upsyncer"] = rawConfig.Contexts["base"].DeepCopy()
	virtualWorkspaceRawConfig.Contexts["upsyncer"].Cluster = "upsyncer"
	syncerVWConfig, err := clientcmd.NewNonInteractiveClientConfig(*virtualWorkspaceRawConfig, "syncer", nil, nil).ClientConfig()
	require.NoError(t, err)
	syncerVWConfig = rest.AddUserAgent(rest.CopyConfig(syncerVWConfig), t.Name())
	require.NoError(t, err)
	upsyncerVWConfig, err := clientcmd.NewNonInteractiveClientConfig(*virtualWorkspaceRawConfig, "upsyncer", nil, nil).ClientConfig()
	require.NoError(t, err)
	upsyncerVWConfig = rest.AddUserAgent(rest.CopyConfig(upsyncerVWConfig), t.Name())
	require.NoError(t, err)

	return &appliedSyncerFixture{
		SyncerConfig:          syncerConfig,
		SyncerID:              syncerID,
		SyncTargetClusterName: syncTargetClusterName,
		DownstreamConfig:      downstreamConfig,
		DownstreamKubeClient:  downstreamKubeClient,

		SyncerVirtualWorkspaceConfig:   syncerVWConfig,
		UpsyncerVirtualWorkspaceConfig: upsyncerVWConfig,
	}
}

// Create creates the SyncTarget and applies it to the physical cluster.
// No resource will be effectively synced after calling this method.
func (sf *syncerFixture) Create(t *testing.T) *appliedSyncerFixture {
	t.Helper()

	artifactDir, _, err := ScratchDirs(t)
	if err != nil {
		t.Errorf("failed to create temp dir for syncer artifacts: %v", err)
	}

	downstreamConfig, _, downstreamKubeClient, syncerConfig, syncerID := sf.createAndApplySyncTarget(t, false, artifactDir)

	ctx, cancelFunc := context.WithCancel(context.Background())
	t.Cleanup(cancelFunc)

	return sf.buildAppliedSyncerFixture(ctx, t, downstreamConfig, downstreamKubeClient, syncerConfig, syncerID)
}

// appliedSyncerFixture contains the configuration required to start a syncer and interact with its
// downstream cluster.
type appliedSyncerFixture struct {
	SyncerConfig          *syncer.SyncerConfig
	SyncerID              string
	SyncTargetClusterName logicalcluster.Name

	// Provide cluster-admin config and client for test purposes. The downstream config in
	// SyncerConfig will be less privileged.
	DownstreamConfig     *rest.Config
	DownstreamKubeClient kubernetesclient.Interface

	SyncerVirtualWorkspaceConfig   *rest.Config
	UpsyncerVirtualWorkspaceConfig *rest.Config

	stopHeartBeat context.CancelFunc
}

// StartHeartBeat starts the Heartbeat keeper to maintain
// the SyncTarget to the Ready state.
// No resource will be effectively synced after calling this method.
func (sf *appliedSyncerFixture) StartHeartBeat(t *testing.T) *StartedSyncerFixture {
	t.Helper()

	ctx, cancelFunc := context.WithCancel(context.Background())
	t.Cleanup(cancelFunc)
	sf.stopHeartBeat = cancelFunc

	kcpBootstrapClusterClient, err := kcpclientset.NewForConfig(sf.SyncerConfig.UpstreamConfig)
	require.NoError(t, err)
	kcpSyncTargetClient := kcpBootstrapClusterClient.Cluster(sf.SyncerConfig.SyncTargetPath)

	// Start the heartbeat keeper to have the SyncTarget always ready during the e2e test.
	syncer.StartHeartbeatKeeper(ctx, kcpSyncTargetClient, sf.SyncerConfig.SyncTargetName, sf.SyncerConfig.SyncTargetUID)

	startedSyncer := &StartedSyncerFixture{
		sf,
	}

	// The sync target becoming ready indicates the syncer is healthy and has
	// successfully sent a heartbeat to kcp.
	startedSyncer.WaitForClusterReady(ctx, t)

	return startedSyncer
}

// StartAPIImporter starts the APIImporter the same way as the Syncer would have done if started.
// This will allow KCP to do the API compatibilitiy checks and update the SyncTarget accordingly.
// The real syncer is not started, and resource will be effectively synced after calling this method.
func (sf *appliedSyncerFixture) StartAPIImporter(t *testing.T) *appliedSyncerFixture {
	t.Helper()

	ctx, cancelFunc := context.WithCancel(context.Background())
	t.Cleanup(cancelFunc)

	kcpBootstrapClusterClient, err := kcpclientset.NewForConfig(sf.SyncerConfig.UpstreamConfig)
	require.NoError(t, err)
	kcpSyncTargetClient := kcpBootstrapClusterClient.Cluster(sf.SyncerConfig.SyncTargetPath)

	// Import the resource schemas of the resources to sync from the physical cludster, to enable compatibility check in KCP.
	resources := sf.SyncerConfig.ResourcesToSync.List()
	kcpSyncTargetInformerFactory := kcpinformers.NewSharedScopedInformerFactoryWithOptions(kcpSyncTargetClient, 10*time.Hour, kcpinformers.WithTweakListOptions(
		func(listOptions *metav1.ListOptions) {
			listOptions.FieldSelector = fields.OneTermEqualSelector("metadata.name", sf.SyncerConfig.SyncTargetName).String()
		},
	))
	kcpImporterInformerFactory := kcpinformers.NewSharedScopedInformerFactoryWithOptions(kcpSyncTargetClient, 10*time.Hour)
	apiImporter, err := syncer.NewAPIImporter(
		sf.SyncerConfig.UpstreamConfig, sf.SyncerConfig.DownstreamConfig,
		kcpSyncTargetInformerFactory.Workload().V1alpha1().SyncTargets(),
		kcpImporterInformerFactory.Apiresource().V1alpha1().APIResourceImports(),
		resources,
		sf.SyncerConfig.SyncTargetPath, sf.SyncerConfig.SyncTargetName, types.UID(sf.SyncerConfig.SyncTargetUID))
	require.NoError(t, err)

	kcpImporterInformerFactory.Start(ctx.Done())
	kcpSyncTargetInformerFactory.Start(ctx.Done())
	kcpSyncTargetInformerFactory.WaitForCacheSync(ctx.Done())

	go apiImporter.Start(klog.NewContext(ctx, klog.FromContext(ctx).WithValues("resources", resources)), 5*time.Second)

	return sf
}

// StartedSyncerFixture contains the configuration used to start a syncer and interact with its
// downstream cluster.
type StartedSyncerFixture struct {
	*appliedSyncerFixture
}

// StopHeartBeat stop maitining the heartbeat for this Syncer SyncTarget.
func (sf *StartedSyncerFixture) StopHeartBeat(t *testing.T) {
	t.Helper()

	sf.stopHeartBeat()
}

// WaitForClusterReady waits for the SyncTarget to be ready.
// The SyncTarget becoming ready indicates that the syncer on the related
// physical cluster is healthy and has successfully sent a heartbeat to kcp.
func (sf *StartedSyncerFixture) WaitForClusterReady(ctx context.Context, t *testing.T) {
	t.Helper()

	cfg := sf.SyncerConfig

	kcpClusterClient, err := kcpclientset.NewForConfig(cfg.UpstreamConfig)
	require.NoError(t, err)
	EventuallyReady(t, func() (conditions.Getter, error) {
		return kcpClusterClient.Cluster(cfg.SyncTargetPath).WorkloadV1alpha1().SyncTargets().Get(ctx, cfg.SyncTargetName, metav1.GetOptions{})
	}, "Waiting for cluster %q condition %q", cfg.SyncTargetName, conditionsv1alpha1.ReadyCondition)
	t.Logf("Cluster %q is %s", cfg.SyncTargetName, conditionsv1alpha1.ReadyCondition)
}

func (sf *StartedSyncerFixture) DownstreamNamespaceFor(t *testing.T, upstreamWorkspace logicalcluster.Name, upstreamNamespace string) string {
	t.Helper()

	desiredNSLocator := shared.NewNamespaceLocator(upstreamWorkspace, sf.SyncTargetClusterName,
		types.UID(sf.SyncerConfig.SyncTargetUID), sf.SyncerConfig.SyncTargetName, upstreamNamespace)
	downstreamNamespaceName, err := shared.PhysicalClusterNamespaceName(desiredNSLocator)
	require.NoError(t, err)
	return downstreamNamespaceName
}

func (sf *StartedSyncerFixture) ToSyncTargetKey() string {
	return workloadv1alpha1.ToSyncTargetKey(sf.SyncTargetClusterName, sf.SyncerConfig.SyncTargetName)
}

// syncerConfigFromCluster reads the configuration needed to start an in-process
// syncer from the resources applied to a cluster for a deployed syncer.
func syncerConfigFromCluster(t *testing.T, downstreamConfig *rest.Config, namespace, syncerID string) *syncer.SyncerConfig {
	t.Helper()

	ctx, cancelFunc := context.WithCancel(context.Background())
	t.Cleanup(cancelFunc)

	downstreamKubeClient, err := kubernetesclient.NewForConfig(downstreamConfig)
	require.NoError(t, err)

	// Read the upstream kubeconfig from the syncer secret
	secret, err := downstreamKubeClient.CoreV1().Secrets(namespace).Get(ctx, syncerID, metav1.GetOptions{})
	require.NoError(t, err)
	upstreamConfigBytes := secret.Data[workloadcliplugin.SyncerSecretConfigKey]
	require.NotEmpty(t, upstreamConfigBytes, "upstream config is required")
	upstreamConfig, err := clientcmd.RESTConfigFromKubeConfig(upstreamConfigBytes)
	require.NoError(t, err, "failed to load upstream config")

	// Read the arguments from the syncer deployment
	deployment, err := downstreamKubeClient.AppsV1().Deployments(namespace).Get(ctx, syncerID, metav1.GetOptions{})
	require.NoError(t, err)
	containers := deployment.Spec.Template.Spec.Containers
	require.NotEmpty(t, containers, "expected at least one container in syncer deployment")
	argMap, err := syncerArgsToMap(containers[0].Args)
	require.NoError(t, err)

	require.NotEmpty(t, argMap["--sync-target-name"], "--sync-target-name is required")
	syncTargetName := argMap["--sync-target-name"][0]
	require.NotEmpty(t, syncTargetName, "a value for --sync-target-name is required")

	require.NotEmpty(t, argMap["--from-cluster"], "--sync-target-name is required")
	fromCluster := argMap["--from-cluster"][0]
	require.NotEmpty(t, fromCluster, "a value for --from-cluster is required")
	syncTargetPath := logicalcluster.NewPath(fromCluster)

	resourcesToSync := argMap["--resources"]
	require.NotEmpty(t, fromCluster, "--resources is required")

	require.NotEmpty(t, argMap["--dns-image"], "--dns-image is required")
	dnsImage := argMap["--dns-image"][0]

	syncTargetUID := argMap["--sync-target-uid"][0]

	// Read the downstream token from the deployment's service account secret
	var tokenSecret corev1.Secret
	Eventually(t, func() (bool, string) {
		secrets, err := downstreamKubeClient.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Errorf("failed to list secrets: %v", err)
			return false, fmt.Sprintf("failed to list secrets downstream: %v", err)
		}
		for _, secret := range secrets.Items {
			t.Logf("checking secret %s/%s for annotation %s=%s", secret.Namespace, secret.Name, corev1.ServiceAccountNameKey, syncerID)
			if secret.Annotations[corev1.ServiceAccountNameKey] == syncerID {
				tokenSecret = secret
				return len(secret.Data["token"]) > 0, fmt.Sprintf("token secret %s/%s for service account %s found", namespace, secret.Name, syncerID)
			}
		}
		return false, fmt.Sprintf("token secret for service account %s/%s not found", namespace, syncerID)
	}, wait.ForeverTestTimeout, time.Millisecond*100, "token secret in namespace %q for syncer service account %q not found", namespace, syncerID)
	token := tokenSecret.Data["token"]
	require.NotEmpty(t, token, "token is required")

	// Compose a new downstream config that uses the token
	downstreamConfigWithToken := ConfigWithToken(string(token), rest.CopyConfig(downstreamConfig))
	return &syncer.SyncerConfig{
		UpstreamConfig:                upstreamConfig,
		DownstreamConfig:              downstreamConfigWithToken,
		ResourcesToSync:               sets.NewString(resourcesToSync...),
		SyncTargetPath:                syncTargetPath,
		SyncTargetName:                syncTargetName,
		SyncTargetUID:                 syncTargetUID,
		DNSImage:                      dnsImage,
		DownstreamNamespaceCleanDelay: 2 * time.Second,
	}
}

// syncerArgsToMap converts the cli argument list from a syncer deployment into a map
// keyed by flags.
func syncerArgsToMap(args []string) (map[string][]string, error) {
	argMap := map[string][]string{}
	for _, arg := range args {
		argParts := strings.SplitN(arg, "=", 2)
		if len(argParts) != 2 {
			return nil, fmt.Errorf("arg %q isn't of the expected form `<key>=<value>`", arg)
		}
		key, value := argParts[0], argParts[1]
		if _, ok := argMap[key]; !ok {
			argMap[key] = []string{value}
		} else {
			argMap[key] = append(argMap[key], value)
		}
	}
	return argMap, nil
}

func endpoints(name, namespace string) *corev1.Endpoints {
	return &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Subsets: []corev1.EndpointSubset{
			{Addresses: []corev1.EndpointAddress{
				{
					IP: "8.8.8.8",
				}}},
		},
	}
}

func service(name, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "8.8.8.8",
		},
	}
}
