/*
Copyright AppsCode Inc. and Contributors

Licensed under the AppsCode Community License 1.0.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://github.com/appscode/licenses/raw/1.0.0/AppsCode-Community-1.0.0.md

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

	"stash.appscode.dev/apimachinery/apis"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"gomodules.xyz/pointer"
	"gomodules.xyz/x/crypto/rand"
	apps "k8s.io/api/apps/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	kutil "kmodules.xyz/client-go"
	apps_util "kmodules.xyz/client-go/apps/v1"
)

func (fi *Invocation) ReplicaSet(name, pvcName, volName string) apps.ReplicaSet {
	name = rand.WithUniqSuffix(fmt.Sprintf("%s-%s", name, fi.app))
	labels := map[string]string{
		"app":  name,
		"kind": "replicaset",
	}
	return apps.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rand.WithUniqSuffix("stash"),
			Namespace: fi.namespace,
			Labels:    labels,
		},
		Spec: apps.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Replicas: pointer.Int32P(1),
			Template: fi.PodTemplate(labels, pvcName, volName),
		},
	}
}

func (f *Framework) CreateReplicaSet(obj apps.ReplicaSet) (*apps.ReplicaSet, error) {
	return f.KubeClient.AppsV1().ReplicaSets(obj.Namespace).Create(context.TODO(), &obj, metav1.CreateOptions{})
}

func (f *Framework) EventuallyReplicaSet(meta metav1.ObjectMeta) GomegaAsyncAssertion {
	return Eventually(func() *apps.ReplicaSet {
		obj, err := f.KubeClient.AppsV1().ReplicaSets(meta.Namespace).Get(context.TODO(), meta.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return obj
	}, WaitTimeOut, PullInterval)
}

func (fi *Invocation) WaitUntilRSReadyWithSidecar(meta metav1.ObjectMeta) error {
	return wait.PollImmediate(kutil.RetryInterval, kutil.ReadinessTimeout, func() (bool, error) {
		if obj, err := fi.KubeClient.AppsV1().ReplicaSets(meta.Namespace).Get(context.TODO(), meta.Name, metav1.GetOptions{}); err == nil {
			if obj.Status.Replicas == obj.Status.ReadyReplicas {
				pods, err := fi.GetAllPods(obj.ObjectMeta)
				if err != nil {
					return false, err
				}
				if len(pods) == 0 {
					return false, nil
				}
				for i := range pods {
					hasSidecar := false
					for _, c := range pods[i].Spec.Containers {
						if c.Name == apis.StashContainer {
							hasSidecar = true
						}
					}
					if !hasSidecar {
						return false, nil
					}
				}
				return true, nil
			}
			return false, nil
		}
		return false, nil
	})
}

func (fi *Invocation) DeployReplicaSet(name string, replica int32, volName string) (*apps.ReplicaSet, error) {
	// append test case specific suffix so that name does not conflict during parallel test
	pvcName := fmt.Sprintf("%s-%s", volName, fi.app)

	// If the PVC does not exist, create PVC for ReplicaSet
	pvc, err := fi.KubeClient.CoreV1().PersistentVolumeClaims(fi.namespace).Get(context.TODO(), pvcName, metav1.GetOptions{})
	if err != nil {
		if kerr.IsNotFound(err) {
			pvc, err = fi.CreateNewPVC(pvcName)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	// Generate ReplicaSet definition
	rs := fi.ReplicaSet(name, pvc.Name, volName)
	rs.Spec.Replicas = &replica

	By("Deploying ReplicaSet: " + rs.Name)
	createdRS, err := fi.CreateReplicaSet(rs)
	if err != nil {
		return createdRS, err
	}
	fi.AppendToCleanupList(createdRS)

	By("Waiting for ReplicaSet to be ready")
	err = apps_util.WaitUntilReplicaSetReady(context.TODO(), fi.KubeClient, createdRS.ObjectMeta)
	Expect(err).NotTo(HaveOccurred())
	// check that we can execute command to the pod.
	// this is necessary because we will exec into the pods and create sample data
	fi.EventuallyAllPodsAccessible(createdRS.ObjectMeta).Should(BeTrue())

	return createdRS, err
}
