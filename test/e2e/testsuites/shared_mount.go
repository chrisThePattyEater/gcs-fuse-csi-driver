/*
Copyright 2018 The Kubernetes Authors.
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package testsuites

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/util"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"local/test/e2e/specs"
)

const (
	sidecarVolName   = "sidecar-gcs-vol"
	sidecarMountPath = "/mnt/sidecar"
	sharedVolName    = "shared-gcs-vol"
	sharedMountPath  = "/mnt/shared"
)

type gcsFuseCSISharedMountTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitGcsFuseCSISharedMountTestSuite returns gcsFuseCSISharedMountTestSuite that implements TestSuite interface.
func InitGcsFuseCSISharedMountTestSuite() storageframework.TestSuite {
	return &gcsFuseCSISharedMountTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "shared-mount",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *gcsFuseCSISharedMountTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *gcsFuseCSISharedMountTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func setupAndDeploySharedMountPod(ctx context.Context, f *framework.Framework, vr *storageframework.VolumeResource, nodeAffinity ...string) (*specs.TestPod, *corev1.Pod) {
	tPod := specs.NewTestPod(f.ClientSet, f.Namespace)
	tPod.SetupVolume(vr, sharedVolName, sharedMountPath, false /* readOnly */)
	if len(nodeAffinity) > 0 && nodeAffinity[0] != "" {
		tPod.SetNodeAffinity(nodeAffinity[0], true /* sameNode */)
	}
	tPod.Create(ctx)
	tPod.WaitForRunning(ctx)

	// Verify client pod does NOT have a sidecar container injected.
	tPod.VerifySidecarPresence(false /* expectPresent */)

	// Verify a single Mounter Pod is created on the same node.
	nodeName := tPod.GetNode()
	specs.VerifyMounterPods(ctx, f.ClientSet, f.Namespace.Name, 1, nodeName)
	mounterPod := specs.GetMounterPod(ctx, f.ClientSet, f.Namespace.Name, nodeName)

	return tPod, mounterPod
}

func (t *gcsFuseCSISharedMountTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		config             *storageframework.PerTestConfig
		volumeResourceList []*storageframework.VolumeResource
	}
	var l local
	ctx := context.Background()

	f := framework.NewFrameworkWithCustomTimeouts("shared-mount", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged

	init := func(volumeNumber int, configPrefix ...string) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		if len(configPrefix) > 0 {
			l.config.Prefix = configPrefix[0]
		}

		l.volumeResourceList = []*storageframework.VolumeResource{}
		for i := range volumeNumber {
			if len(configPrefix) > 0 && configPrefix[0] == specs.SidecarAndSharedMountCoexistencePrefix && i == 0 {
				// Volume 0: Sidecar-mode volume (CSI ephemeral inline)
				l.volumeResourceList = append(l.volumeResourceList, storageframework.CreateVolumeResource(ctx, driver, l.config, storageframework.DefaultFsCSIEphemeralVolume, e2evolume.SizeRange{}))
				continue
			}
			l.volumeResourceList = append(l.volumeResourceList, specs.CreateVolumeResource(ctx, driver, l.config, pattern, e2evolume.SizeRange{}))
		}
	}

	cleanup := func() {
		var cleanUpErrs []error
		for _, vr := range l.volumeResourceList {
			if vr != nil {
				if err := vr.CleanupResource(ctx); err != nil {
					cleanUpErrs = append(cleanUpErrs, err)
				}
			}
		}
		if len(cleanUpErrs) > 0 {
			err := utilerrors.NewAggregate(cleanUpErrs)
			framework.ExpectNoError(err, "while cleaning up")
		}
	}

	// TC: Sidecar and Shared Mount Coexistence Test
	// Verify that a pod using a GCSFuse sidecar-mode volume and a pod using a shared-mount volume
	// can coexist on the same node without conflicts. Also verify the webhook rejects a pod that mixes
	// sidecar and shared-mount volumes.
	ginkgo.It("[shared-mount] should verify sidecar and shared mount coexistence on the same node and reject pod mixing both volume types", func() {
		init(2, specs.SidecarAndSharedMountCoexistencePrefix)
		defer cleanup()

		gomega.Expect(l.volumeResourceList).To(gomega.HaveLen(2))
		gomega.Expect(l.volumeResourceList[0]).ToNot(gomega.BeNil())
		gomega.Expect(l.volumeResourceList[1]).ToNot(gomega.BeNil())

		sidecarVR := l.volumeResourceList[0]
		sharedVR := l.volumeResourceList[1]

		// 2. Attempt to create a single Pod referencing both volumes, and verify that the mutating webhook rejects the Pod creation attempt.
		ginkgo.By("Attempting to create a single Pod referencing both sidecar and shared-mount volumes")
		mixedPod := specs.NewTestPod(f.ClientSet, f.Namespace)
		mixedPod.SetupVolume(sidecarVR, sidecarVolName, sidecarMountPath, false /* readOnly */)
		mixedPod.SetupVolume(sharedVR, sharedVolName, sharedMountPath, false /* readOnly */)

		ginkgo.By("Verifying that the mutating webhook rejects the mixed Pod creation attempt")
		mixedPod.CreateExpectErrorContaining(ctx, "mixing shared node mount and non-shared node mount GCSFuse volumes in the same Pod is not allowed")

		// 3. Create sidecarTestPod referencing the sidecar volume and sharedMountTestPod referencing the shared mount PVC on the same node.
		ginkgo.By("Configuring and deploying sidecarTestPod referencing the sidecar volume")
		sidecarTestPod := specs.NewTestPod(f.ClientSet, f.Namespace)
		sidecarTestPod.SetupVolume(sidecarVR, sidecarVolName, sidecarMountPath, false /* readOnly */)
		sidecarTestPod.Create(ctx)
		defer sidecarTestPod.Cleanup(ctx)

		ginkgo.By("Waiting for sidecarTestPod to be running and getting its node")
		sidecarTestPod.WaitForRunning(ctx)
		nodeName := sidecarTestPod.GetNode()

		ginkgo.By(fmt.Sprintf("Configuring and deploying sharedMountTestPod referencing the shared mount PVC on node %s", nodeName))
		sharedMountTestPod, _ := setupAndDeploySharedMountPod(ctx, f, sharedVR, nodeName)
		defer sharedMountTestPod.Cleanup(ctx)
		gomega.Expect(sharedMountTestPod.GetNode()).To(gomega.Equal(nodeName), "expected sharedMountTestPod to run on the same node as sidecarTestPod")

		// 4. Verify sidecarTestPod has a sidecar container injected.
		ginkgo.By("Verifying sidecarTestPod has a sidecar container injected")
		sidecarTestPod.VerifySidecarPresence(true /* expectPresent */)

		// 5. Verify that both pods can successfully read and write to their respective volumes without conflicts.
		ginkgo.By("Verifying sidecarTestPod can write and read from its sidecar-mounted volume")
		sidecarTestPod.VerifyRWMount(f, sidecarMountPath)
		sidecarTestPod.VerifyWriteAndReadFile(f, fmt.Sprintf("%s/data-sidecar", sidecarMountPath), "hello from sidecar pod")

		ginkgo.By("Verifying sharedMountTestPod can write and read from its shared-mounted volume")
		sharedMountTestPod.VerifyRWMount(f, sharedMountPath)
		sharedMountTestPod.VerifyWriteAndReadFile(f, fmt.Sprintf("%s/data-shared", sharedMountPath), "hello from shared mount pod")

		ginkgo.By("Verifying data persistence and isolation on both volumes")
		sidecarTestPod.VerifyReadFile(f, fmt.Sprintf("%s/data-sidecar", sidecarMountPath), "hello from sidecar pod")
		sharedMountTestPod.VerifyReadFile(f, fmt.Sprintf("%s/data-shared", sharedMountPath), "hello from shared mount pod")
	})

	// TC: Dynamic Mounting Test
	// Verify that dynamic mounting works with the shared node mount architecture.
	// Create a PV with volumeHandle: _ and sharedMount: true.
	// Create multiple Pods referencing the PVC.
	// Verify the Mounter Pod is created.
	// Verify all pods can successfully read from and write to any buckets their KSA is authorized to access.
	ginkgo.It("[shared-mount] should verify dynamic mounting across multiple pods with volumeHandle _", func() {
		init(1, specs.SharedDynamicMountPrefix)
		defer cleanup()

		gomega.Expect(l.volumeResourceList).To(gomega.HaveLen(1))
		gomega.Expect(l.volumeResourceList[0]).ToNot(gomega.BeNil())

		sharedVR := l.volumeResourceList[0]
		buckets := strings.Split(l.config.Prefix, ",")
		gomega.Expect(buckets).To(gomega.HaveLen(2), "expected 2 buckets created for dynamic mounting")

		// 1. Configure and deploy Pod 1 referencing the dynamic shared-mount PVC.
		ginkgo.By("Configuring and deploying the first pod referencing the dynamic shared-mount PVC")
		tPod1, _ := setupAndDeploySharedMountPod(ctx, f, sharedVR)
		defer tPod1.Cleanup(ctx)
		nodeName := tPod1.GetNode()

		// 2. Configure and deploy Pod 2 on the same node referencing the same PVC.
		ginkgo.By(fmt.Sprintf("Configuring and deploying the second pod on node %s referencing the same PVC", nodeName))
		tPod2, _ := setupAndDeploySharedMountPod(ctx, f, sharedVR, nodeName)
		defer tPod2.Cleanup(ctx)
		gomega.Expect(tPod2.GetNode()).To(gomega.Equal(nodeName), "expected second pod to run on the same node as first pod")

		// 3. Verify RW mount point in both pods.
		ginkgo.By("Verifying RW mount point in both pods")
		tPod1.VerifyRWMount(f, sharedMountPath)
		tPod2.VerifyRWMount(f, sharedMountPath)

		// 4. Verify dynamic multi-bucket read and write operations across both pods.
		ginkgo.By("Verifying both pods can read and write across all authorized buckets")
		for _, bucket := range buckets {
			pod1File := fmt.Sprintf("%s/%s/pod1-data.txt", sharedMountPath, bucket)
			pod2File := fmt.Sprintf("%s/%s/pod2-data.txt", sharedMountPath, bucket)
			pod1Content := fmt.Sprintf("hello from pod1 in bucket %s", bucket)
			pod2Content := fmt.Sprintf("hello from pod2 in bucket %s", bucket)

			// Pod 1 writes and reads its own file in this bucket
			tPod1.VerifyWriteAndReadFile(f, pod1File, pod1Content)

			// Pod 2 writes and reads its own file in this bucket
			tPod2.VerifyWriteAndReadFile(f, pod2File, pod2Content)

			// Cross-pod read: Pod 1 reads Pod 2's file, Pod 2 reads Pod 1's file
			tPod1.VerifyReadFile(f, pod2File, pod2Content)
			tPod2.VerifyReadFile(f, pod1File, pod1Content)
		}
	})

	// TC: Kernel Parameters with Shared Mount Test
	// Verify that kernel parameters (read_ahead_kb, kernel-params.json) are correctly applied
	// when using shared mount. The kernel params file should be created in the Mounter Pod's emptyDir,
	// not the customer Pod's.
	// 1. Create a PodTemplate and a PV/PVC with sharedMount: true and a custom read_ahead_kb mount option.
	// 2. Create a workload pod referencing the PVC.
	// 3. Verify that kernel-params.json is created inside the Mounter Pod's gke-gcsfuse-tmp emptyDir volume
	//    instead of the workload pod's volume.
	// 4. Verify that the CSI Node driver detects kernel-params.json and updates the host node kernel parameters.
	// 5. Verify that the workload pod can successfully read and write data to the volume.
	ginkgo.It("[shared-mount] should verify kernel parameters are applied via mounter pod and host node settings are updated", func() {
		skipIfKernelParamsNotSupported()
		init(1, specs.EnableCustomReadAhead)
		defer cleanup()

		gomega.Expect(l.volumeResourceList).To(gomega.HaveLen(1))
		gomega.Expect(l.volumeResourceList[0]).ToNot(gomega.BeNil())

		sharedVR := l.volumeResourceList[0]

		// 1. Configure and deploy the workload pod referencing the PVC.
		ginkgo.By("Configuring and deploying the workload pod referencing the shared-mount PVC")
		workloadPod, mounterPod := setupAndDeploySharedMountPod(ctx, f, sharedVR)
		defer workloadPod.Cleanup(ctx)

		// 2. Verify kernel-params.json is created inside the Mounter Pod's gke-gcsfuse-tmp emptyDir volume.
		ginkgo.By("Verifying kernel-params.json is created inside the Mounter Pod's gke-gcsfuse-tmp volume")
		mounterKernelParamsPath := fmt.Sprintf("/gcsfuse-tmp/.volumes/%s/kernel-params.json", sharedVR.Pv.Name)
		var configData string
		gomega.Eventually(func() error {
			out, _, execErr := e2epod.ExecCommandInContainerWithFullOutput(f, mounterPod.Name, util.MounterPodNamePrefix, "/bin/sh", "-c", fmt.Sprintf("cat %s", mounterKernelParamsPath))
			if execErr != nil {
				return execErr
			}
			configData = out
			return nil
		}, retryTimeout, retryPolling).Should(gomega.Succeed(), "failed to read kernel-params.json in Mounter Pod")

		// Verify kernel-params.json contains max-read-ahead-kb matching specs.ReadAheadCustomReadAheadKb.
		pattern := fmt.Sprintf(kernelParamExtractRegex, regexp.QuoteMeta(string(MaxReadAheadKb)))
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(configData)
		gomega.Expect(matches).To(gomega.HaveLen(2), "expected to extract max-read-ahead-kb from kernel-params.json")
		gomega.Expect(matches[1]).To(gomega.Equal(specs.ReadAheadCustomReadAheadKb), "expected max-read-ahead-kb in kernel-params.json to match custom value")

		// 3. Verify kernel-params.json is NOT in the workload pod's volume or filesystem.
		ginkgo.By("Verifying kernel-params.json is NOT created inside the workload pod")
		workloadPod.VerifyExecInPodFail(f, specs.TesterContainerName, "ls /gcsfuse-tmp", 1)
		workloadPod.VerifyExecInPodFail(f, specs.TesterContainerName, fmt.Sprintf("ls %s/kernel-params.json", sharedMountPath), 1)

		// 4. Verify CSI Node driver detects kernel-params.json and updates the host node kernel parameters.
		ginkgo.By("Verifying host node read_ahead_kb kernel parameter is updated to the custom value")
		gomega.Eventually(func() string {
			bdi := workloadPod.VerifyExecInPodSucceedWithOutput(f, specs.TesterContainerName, fmt.Sprintf(`mountpoint -d "%s"`, sharedMountPath))
			readAheadPath := fmt.Sprintf("/sys/class/bdi/%s/read_ahead_kb", strings.TrimSpace(bdi))
			return strings.TrimSpace(workloadPod.VerifyExecInPodSucceedWithOutput(f, specs.TesterContainerName, "cat "+readAheadPath))
		}, retryTimeout, retryPolling).Should(gomega.Equal(specs.ReadAheadCustomReadAheadKb))

		// 5. Verify workload pod can read and write data to the volume.
		ginkgo.By("Verifying workload pod can write and read from its shared-mounted volume")
		workloadPod.VerifyRWMount(f, sharedMountPath)
		testFilePath := fmt.Sprintf("%s/kernel-params-test-data.txt", sharedMountPath)
		testContent := "hello from shared mount kernel params test"
		workloadPod.VerifyWriteAndReadFile(f, testFilePath, testContent)
	})
}
