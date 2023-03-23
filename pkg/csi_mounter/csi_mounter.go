/*
Copyright 2022 Google LLC

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

package csimounter

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	sidecarmounter "github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/sidecar_mounter"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/util"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/webhook"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

// Mounter provides the Cloud Storage FUSE CSI implementation of mount.Interface
// for the linux platform.
type Mounter struct {
	mount.MounterForceUnmounter
	chdirMu sync.Mutex
}

// New returns a mount.MounterForceUnmounter for the current system.
// It provides options to override the default mounter behavior.
// mounterPath allows using an alternative to `/bin/mount` for mounting.
func New(mounterPath string) mount.Interface {
	return &Mounter{
		mount.New(mounterPath).(mount.MounterForceUnmounter),
		sync.Mutex{},
	}
}

func (m *Mounter) Mount(source string, target string, fstype string, options []string) error {

	// Prepare the mount options
	mountOptions := []string{
		"nodev",
		"nosuid",
		"noexec",
		"default_permissions",
		"rootmode=40000",
		fmt.Sprintf("user_id=%d", os.Getuid()),
		fmt.Sprintf("group_id=%d", os.Getgid()),
	}

	// users may pass options that should be used by Linux mount(8),
	// filter out these options and not pass to the sidecar mounter.
	validMountOptions := []string{"rw", "ro", "allow_other"}
	optionSet := sets.NewString(options...)
	for _, o := range validMountOptions {
		if optionSet.Has(o) {
			mountOptions = append(mountOptions, options[0])
			optionSet.Delete(o)
		}
	}
	options = optionSet.List()

	// Prepare the temp emptyDir path
	emptyDirBasePath, err := util.PrepareEmptyDir(target, false)
	if err != nil {
		return fmt.Errorf("failed to prepare emptyDir path: %v", err)
	}

	klog.V(4).Info("opening the device /dev/fuse")
	fd, err := syscall.Open("/dev/fuse", syscall.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open the device /dev/fuse: %v", err)
	}
	mountOptions = append(mountOptions, fmt.Sprintf("fd=%v", fd))

	klog.V(4).Info("mounting the fuse filesystem")
	err = m.MountSensitiveWithoutSystemdWithMountFlags(source, target, "fuse", mountOptions, nil, []string{"--internal-only"})
	if err != nil {
		return fmt.Errorf("failed to mount the fuse filesystem: %v", err)
	}

	klog.V(4).Info("passing the descriptor")
	// Need to change the current working directory to the temp volume base path,
	// because the socket absolute path is longer than 104 characters,
	// which will cause "bind: invalid argument" errors.
	m.chdirMu.Lock()
	exPwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get the current directory to %v", err)
	}
	if err = os.Chdir(emptyDirBasePath); err != nil {
		return fmt.Errorf("failed to change directory to %q: %v", emptyDirBasePath, err)
	}

	klog.V(4).Info("creating a listener for the socket")
	l, err := net.Listen("unix", "./socket")
	if err != nil {
		return fmt.Errorf("failed to create the listener for the socket: %v", err)
	}

	// Change the socket ownership
	err = os.Chown(filepath.Dir(emptyDirBasePath), webhook.NobodyUID, webhook.NobodyGID)
	if err != nil {
		return fmt.Errorf("failed to change ownership on base of emptyDirBasePath: %v", err)
	}
	err = os.Chown(emptyDirBasePath, webhook.NobodyUID, webhook.NobodyGID)
	if err != nil {
		return fmt.Errorf("failed to change ownership on emptyDirBasePath: %v", err)
	}
	err = os.Chown("./socket", webhook.NobodyUID, webhook.NobodyGID)
	if err != nil {
		return fmt.Errorf("failed to change ownership on socket: %v", err)
	}

	// Close the listener after 15 minutes
	// TODO: properly handle the socket listener timed out
	go func(l net.Listener) {
		time.Sleep(15 * time.Minute)
		l.Close()
	}(l)

	if err = os.Chdir(exPwd); err != nil {
		return fmt.Errorf("failed to change directory to %q: %v", exPwd, err)
	}
	m.chdirMu.Unlock()

	// Prepare sidecar mounter MountConfig
	mc := sidecarmounter.MountConfig{
		BucketName: source,
		Options:    options,
	}
	mcb, err := json.Marshal(mc)
	if err != nil {
		return fmt.Errorf("failed to marshal sidecar mounter MountConfig %v: %v", mc, err)
	}

	// Asynchronously waiting for the sidecar container to connect to the listener
	go func(l net.Listener, bucketName, target string, msg []byte, fd int) {
		defer syscall.Close(fd)
		defer l.Close()

		podID, volumeName, _ := util.ParsePodIDVolumeFromTargetpath(target)
		logPrefix := fmt.Sprintf("[Pod %v, Volume %v, Bucket %v]", podID, volumeName, bucketName)

		klog.V(4).Infof("%v start to accept connections to the listener.", logPrefix)
		a, err := l.Accept()
		if err != nil {
			klog.Errorf("%v failed to accept connections to the listener: %v", logPrefix, err)
			return
		}
		defer a.Close()

		klog.V(4).Infof("%v start to send file descriptor and mount options", logPrefix)
		if err = util.SendMsg(a, fd, msg); err != nil {
			klog.Errorf("%v failed to send file descriptor and mount options: %v", logPrefix, err)
		}

		klog.V(4).Infof("%v exiting the goroutine.", logPrefix)
	}(l, source, target, mcb, fd)

	return nil
}
