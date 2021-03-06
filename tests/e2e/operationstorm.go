/*
Copyright 2019 The Kubernetes Authors.

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

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/storage/utils"
)

/*
	Test to perform volume Ops storm.

	Steps
    	1. Create storage class for dynamic volume provisioning using CSI driver.
    	2. Create PVCs using above storage class in annotation, requesting 2 GB volume.
    	3. Wait until all disks are ready and all PVs and PVCs get bind. (CreateVolume storm)
    	4. Create pod to mount volumes using PVCs created in step 2. (AttachDisk storm)
    	5. Wait for pod status to be running.
    	6. Verify all volumes accessible and available in the pod.
    	7. Delete pod.
    	8. wait until volumes gets detached. (DetachDisk storm)
    	9. Delete all PVCs. This should delete all Disks. (DeleteVolume storm)
		10. Delete storage class.
*/

var _ = utils.SIGDescribe("[csi-block-e2e] Volume Operations Storm", func() {
	f := framework.NewDefaultFramework("volume-ops-storm")
	const defaultVolumeOpsScale = 30
	var (
		client            clientset.Interface
		namespace         string
		storageclass      *storage.StorageClass
		pvclaims          []*v1.PersistentVolumeClaim
		persistentvolumes []*v1.PersistentVolume
		err               error
		volumeOpsScale    int
	)
	ginkgo.BeforeEach(func() {
		client = f.ClientSet
		namespace = f.Namespace.Name
		nodeList := framework.GetReadySchedulableNodesOrDie(f.ClientSet)
		if !(len(nodeList.Items) > 0) {
			framework.Failf("Unable to find ready and schedulable Node")
		}
		bootstrap()
		if os.Getenv("VOLUME_OPS_SCALE") != "" {
			volumeOpsScale, err = strconv.Atoi(os.Getenv(envVolumeOperationsScale))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		} else {
			volumeOpsScale = defaultVolumeOpsScale
		}
		pvclaims = make([]*v1.PersistentVolumeClaim, volumeOpsScale)
	})

	ginkgo.AfterEach(func() {
		ginkgo.By("Deleting all PVCs")
		for _, claim := range pvclaims {
			framework.DeletePersistentVolumeClaim(client, claim.Name, namespace)
		}
		ginkgo.By("Wait until all PVs are deleted from Kubernetes and CNS")
		for _, pv := range persistentvolumes {
			framework.WaitForPersistentVolumeDeleted(client, pv.Name, framework.Poll, framework.PodDeleteTimeout)
			e2eVSphere.waitForCNSVolumeToBeDeleted(pv.Spec.CSI.VolumeHandle)
		}
	})

	ginkgo.It("create/delete pod with many volumes and verify no attach/detach call should fail", func() {
		ginkgo.By(fmt.Sprintf("Running test with VOLUME_OPS_SCALE: %v", volumeOpsScale))
		ginkgo.By("Creating Storage Class")
		storageclass, err = client.StorageV1().StorageClasses().Create(getVSphereStorageClassSpec("", nil, nil, "", ""))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer client.StorageV1().StorageClasses().Delete(storageclass.Name, nil)

		ginkgo.By("Creating PVCs using the Storage Class")
		count := 0
		for count < volumeOpsScale {
			pvclaims[count], err = framework.CreatePVC(client, namespace, getPersistentVolumeClaimSpecWithStorageClass(namespace, diskSize, storageclass, nil))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			count++
		}

		ginkgo.By("Waiting for all claims to be in bound state")
		persistentvolumes, err = framework.WaitForPVClaimBoundPhase(client, pvclaims, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Creating pod to attach PVs to the node")
		pod, err := framework.CreatePod(client, namespace, nil, pvclaims, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer client.CoreV1().Pods(namespace).Delete(pod.Name, nil)

		ginkgo.By("Verify the volumes are attached to the node vm")
		for _, pv := range persistentvolumes {
			ginkgo.By(fmt.Sprintf("Verify volume:%s is attached to the node: %s", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
			isDiskAttached, err := e2eVSphere.isVolumeAttachedToNode(client, pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(isDiskAttached).To(gomega.BeTrue(), fmt.Sprintf("Volume: %s is not attached to the node: %s", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
		}

		ginkgo.By("Verify all volumes are accessible in the pod")
		for index := range persistentvolumes {
			// Verify Volumes are accessible by creating an empty file on the volume
			filepath := filepath.Join("/mnt/", fmt.Sprintf("volume%v", index+1), "/emptyFile.txt")
			_, err = framework.LookForStringInPodExec(namespace, pod.Name, []string{"/bin/touch", filepath}, "", time.Minute)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		ginkgo.By("Deleting pod")
		framework.ExpectNoError(framework.DeletePodWithWait(f, client, pod))

		ginkgo.By("Verify volumes are detached from the node")
		for _, pv := range persistentvolumes {
			isDiskDetached, err := e2eVSphere.waitForVolumeDetachedFromNode(client, pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(isDiskDetached).To(gomega.BeTrue(), fmt.Sprintf("Volume %q is not detached from the node %q", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
		}
		ginkgo.By("Deleting PVCs")
		for _, claim := range pvclaims {
			err = framework.DeletePersistentVolumeClaim(client, claim.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		ginkgo.By("Verify volumes are deleted from CNS")
		for _, pv := range persistentvolumes {
			err = e2eVSphere.waitForCNSVolumeToBeDeleted(pv.Spec.CSI.VolumeHandle)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), fmt.Sprintf("Volume: %s should not present in the CNS after it is deleted from kubernetes", pv.Spec.CSI.VolumeHandle))
		}
	})
})
