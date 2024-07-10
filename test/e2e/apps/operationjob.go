/*
Copyright 2024 The KusionStack Authors.

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

package apps

import (
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "kusionstack.io/operating/apis/apps/v1alpha1"
	"kusionstack.io/operating/test/e2e/framework"
)

var _ = SIGDescribe("OperationJob", func() {

	f := framework.NewDefaultFramework("operationjob")
	var client client.Client
	var ns string
	var clientSet clientset.Interface
	var clsTester *framework.CollaSetTester
	var ojTester *framework.OperationJobTester
	var randStr string

	BeforeEach(func() {
		clientSet = f.ClientSet
		client = f.Client
		ns = f.Namespace.Name
		clsTester = framework.NewCollaSetTester(clientSet, client, ns)
		ojTester = framework.NewOperationJobTester(clientSet, client, ns)
		randStr = rand.String(10)
	})

	framework.KusionstackDescribe("OperationJob Replacing", func() {

		framework.ConformanceIt("operationjob replace pod", func() {
			cls := clsTester.NewCollaSet("collaset-"+randStr, 1, appsv1alpha1.UpdateStrategy{})
			Expect(clsTester.CreateCollaSet(cls)).NotTo(HaveOccurred())

			By("Wait for CollaSet status replicas satisfied")
			Eventually(func() error { return clsTester.ExpectedStatusReplicas(cls, 1, 1, 1, 1, 1) }, 30*time.Second, 3*time.Second).ShouldNot(HaveOccurred())
			pods, err := clsTester.ListPodsForCollaSet(cls)
			Expect(err).NotTo(HaveOccurred())

			By("Create replace OperationJob")
			replaceOriginPod := pods[0]
			oj := ojTester.NewOperationJob("operationjob-"+randStr, appsv1alpha1.OpsActionReplace, []appsv1alpha1.PodOpsTarget{
				{
					PodName: replaceOriginPod.Name,
				},
			})
			Expect(ojTester.CreateOperationJob(oj)).NotTo(HaveOccurred())

			By("Wait for replace OperationJob Finished")
			Eventually(func() error { return ojTester.ExpectOperationJobProgress(oj, appsv1alpha1.OperationProgressSucceeded) }, 30*time.Second, 3*time.Second).ShouldNot(HaveOccurred())
		})
	})

})
