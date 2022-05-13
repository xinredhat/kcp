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

package syncer

import (
	"context"
	"embed"
	"testing"
	"time"

	"github.com/kcp-dev/logicalcluster"
	"github.com/stretchr/testify/require"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	kubernetesclientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	kyaml "sigs.k8s.io/yaml"

	"github.com/kcp-dev/kcp/pkg/syncer/shared"
	"github.com/kcp-dev/kcp/test/e2e/framework"
)

//go:embed *.yaml
var embeddedResources embed.FS

func TestSyncerLifecycle(t *testing.T) {
	t.Parallel()

	upstreamServer := framework.SharedKcpServer(t)

	t.Log("Creating an organization")
	orgClusterName := framework.NewOrganizationFixture(t, upstreamServer)

	t.Log("Creating a workspace")
	wsClusterName := framework.NewWorkspaceFixture(t, upstreamServer, orgClusterName, "Universal")

	// The Start method of the fixture will initiate syncer start and then wait for
	// its workload cluster to go ready. This implicitly validates the syncer
	// heartbeating and the heartbeat controller setting the workload cluster ready in
	// response.
	syncerFixture := framework.SyncerFixture{
		UpstreamServer:       upstreamServer,
		WorkspaceClusterName: wsClusterName,
	}.Start(t)

	ctx, cancelFunc := context.WithCancel(context.Background())
	t.Cleanup(cancelFunc)

	upstreamConfig := upstreamServer.DefaultConfig(t)
	upstreamKubeClusterClient, err := kubernetesclientset.NewClusterForConfig(upstreamConfig)
	require.NoError(t, err)
	upstreamKubeClient := upstreamKubeClusterClient.Cluster(wsClusterName)

	t.Log("Creating upstream namespace...")
	upstreamNamespace, err := upstreamKubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-syncer",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	downstreamKubeClient, err := kubernetesclientset.NewForConfig(syncerFixture.DownstreamConfig)
	require.NoError(t, err)

	// Determine downstream name of the namespace
	nsLocator := shared.NamespaceLocator{LogicalCluster: logicalcluster.From(upstreamNamespace), Namespace: upstreamNamespace.Name}
	downstreamNamespaceName, err := shared.PhysicalClusterNamespaceName(nsLocator)
	require.NoError(t, err)

	// TODO(marun) The name mapping should be defined for reuse outside of the transformName method in pkg/syncer
	serviceAccountName := "kcp-default"
	secretName := "kcp-default-token"
	configMapName := "kcp-root-ca.crt"

	t.Logf("Waiting for downstream service account %s/%s to be created...", downstreamNamespaceName, serviceAccountName)
	require.Eventually(t, func() bool {
		// TODO(marun) The name mapping should be defined for reuse outside of the transformName method in pkg/syncer
		_, err = downstreamKubeClient.CoreV1().ServiceAccounts(downstreamNamespaceName).Get(ctx, serviceAccountName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false
		}
		if err != nil {
			t.Errorf("saw an error waiting for downstream service account %s/%s to be created: %v", downstreamNamespaceName, serviceAccountName, err)
			return false
		}
		return true
	}, wait.ForeverTestTimeout, time.Millisecond*100, "downstream service account %s/%s was not created", downstreamNamespaceName, serviceAccountName)

	t.Logf("Waiting for downstream service account secret %s/%s to be created...", downstreamNamespaceName, secretName)
	require.Eventually(t, func() bool {
		_, err = downstreamKubeClient.CoreV1().Secrets(downstreamNamespaceName).Get(ctx, secretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false
		}
		if err != nil {
			t.Errorf("saw an error waiting for downstream service account secret %s/%s to be created: %v", downstreamNamespaceName, secretName, err)
			return false
		}
		return true
	}, wait.ForeverTestTimeout, time.Millisecond*100, "downstream service account secret %s/%s was not created", downstreamNamespaceName, secretName)

	t.Logf("Waiting for downstream configmap %s/%s to be created...", downstreamNamespaceName, configMapName)
	require.Eventually(t, func() bool {
		_, err = downstreamKubeClient.CoreV1().ConfigMaps(downstreamNamespaceName).Get(ctx, configMapName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false
		}
		if err != nil {
			t.Errorf("saw an error waiting for downstream configmap %s/%s to be created: %v", downstreamNamespaceName, configMapName, err)
			return false
		}
		return true
	}, wait.ForeverTestTimeout, time.Millisecond*100, "downstream configmap %s/%s was not created", downstreamNamespaceName, configMapName)

	t.Log("Creating upstream deployment...")

	deploymentYAML, err := embeddedResources.ReadFile("deployment.yaml")
	require.NoError(t, err, "failed to read embedded deployment")

	var deployment *appsv1.Deployment
	err = yaml.Unmarshal(deploymentYAML, &deployment)
	require.NoError(t, err, "failed to unmarshal deployment")

	// This test created a new workspace that initially lacked support for deployments, but once the
	// workload cluster went ready (checked by the syncer fixture's Start method) the api importer
	// will have enabled deployments in the logical cluster.
	upstreamDeployment, err := upstreamKubeClient.AppsV1().Deployments(upstreamNamespace.Name).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create deployment")

	t.Logf("Waiting for downstream deployment %s/%s to be created...", downstreamNamespaceName, upstreamDeployment.Name)
	require.Eventually(t, func() bool {
		deployment, err = downstreamKubeClient.AppsV1().Deployments(downstreamNamespaceName).Get(ctx, upstreamDeployment.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false
		}
		if err != nil {
			t.Errorf("saw an error waiting for downstream deployment %s/%s to be created: %v", downstreamNamespaceName, upstreamDeployment.Name, err)
		}
		return true
	}, wait.ForeverTestTimeout, time.Millisecond*100, "downstream deployment %s/%s was not synced", downstreamNamespaceName, upstreamDeployment.Name)

	if len(framework.TestConfig.PClusterKubeconfig()) > 0 {
		t.Logf("Check for available replicas if downstream is capable of actually running the deployment")
		expectedAvailableReplicas := int32(1)
		require.Eventually(t, func() bool {
			deployment, err = downstreamKubeClient.AppsV1().Deployments(downstreamNamespaceName).Get(ctx, upstreamDeployment.Name, metav1.GetOptions{})
			require.NoError(t, err)
			klog.Info(toYaml(deployment))
			return expectedAvailableReplicas == deployment.Status.AvailableReplicas
		}, wait.ForeverTestTimeout, time.Millisecond*100, "downstream deployment %s/%s didn't get available", downstreamNamespaceName, upstreamDeployment.Name)

		t.Logf("Create upstream service account permissions for downstream in-cluster configuration test")
		roleName := "configmap-admin"
		_, err := upstreamKubeClient.RbacV1().Roles(upstreamNamespace.Name).Create(ctx, &v1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name: roleName,
			},
			Rules: []v1.PolicyRule{
				{
					Verbs:     []string{"*"},
					APIGroups: []string{""},
					Resources: []string{"configmaps"},
				},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err, "failed to create upstream role")

		_, err = upstreamKubeClient.RbacV1().RoleBindings(upstreamNamespace.Name).Create(ctx, &v1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "default:configmap-admin",
			},
			RoleRef: v1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     roleName,
			},
			Subjects: []v1.Subject{
				{
					Kind: "ServiceAccount",
					Name: "default",
				},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err, "failed to create upstream rolebinding")

		t.Logf("Creating upstream in-cluster configuration test deployment")
		expectedConfigMapName := "expected-configmap"
		iccUpstreamDeployment, err := upstreamKubeClient.AppsV1().Deployments(upstreamNamespace.Name).Create(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: "icc-test",
			},
			Spec: appsv1.DeploymentSpec{
				//Replicas: 1,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "icc-test",
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "icc-test",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "icc-test",
								Image: framework.TestConfig.InClusterConfigTestImage(),
								Env: []corev1.EnvVar{
									{Name: "TARGET_NAMESPACE", Value: upstreamNamespace.Name},
									{Name: "CONFIGMAP_NAME", Value: expectedConfigMapName},
								},
							},
						},
					},
				},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err, "failed to create icc-test deployment")

		t.Logf("Waiting for downstream in-cluster config test deployment %s/%s to be created...", downstreamNamespaceName, iccUpstreamDeployment.Name)
		require.Eventually(t, func() bool {
			deployment, err = downstreamKubeClient.AppsV1().Deployments(downstreamNamespaceName).Get(ctx, iccUpstreamDeployment.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false
			}
			if err != nil {
				t.Errorf("saw an error waiting for downstream deployment %s/%s to be created: %v", downstreamNamespaceName, iccUpstreamDeployment.Name, err)
			}
			return true
		}, wait.ForeverTestTimeout, time.Millisecond*100, "downstream deployment %s/%s was not synced", downstreamNamespaceName, iccUpstreamDeployment.Name)

		t.Logf("Waiting for configmap generated by icc-test deployment to show up upstream")
		require.Eventually(t, func() bool {
			_, err := upstreamKubeClient.CoreV1().ConfigMaps(upstreamNamespace.Name).Get(ctx, expectedConfigMapName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false
			}
			if err != nil {
				t.Errorf("saw an error waiting for upstream configmap %s/%s to be created: %v", upstreamNamespace.Name, expectedConfigMapName, err)
			}
			return true
		}, wait.ForeverTestTimeout, time.Millisecond*100, "upstream configmap %s/%s was not found", upstreamNamespace.Name, expectedConfigMapName)

	}
}

func toYaml(obj interface{}) string {
	b, err := kyaml.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestSyncWorkload(t *testing.T) {
	t.Parallel()

	workloadClusterName := "test-wlc"
	upstreamServer := framework.SharedKcpServer(t)

	t.Log("Creating an organization")
	orgClusterName := framework.NewOrganizationFixture(t, upstreamServer)

	t.Log("Creating a workspace")
	wsClusterName := framework.NewWorkspaceFixture(t, upstreamServer, orgClusterName, "Universal")

	// Write the upstream logical cluster config to disk for the workspace plugin
	upstreamRawConfig, err := upstreamServer.RawConfig()
	require.NoError(t, err)
	_, kubeconfigPath := framework.WriteLogicalClusterConfig(t, upstreamRawConfig, wsClusterName)

	subCommand := []string{
		"workload",
		"sync",
		workloadClusterName,
		"--syncer-image",
		"ghcr.io/kcp-dev/kcp/syncer-c2e3073d5026a8f7f2c47a50c16bdbec:41ca72b",
	}

	framework.RunKcpCliPlugin(t, kubeconfigPath, subCommand)

	framework.RunKcpCliPlugin(t, kubeconfigPath, subCommand)
}
