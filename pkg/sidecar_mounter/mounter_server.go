/*
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

package sidecarmounter

import (
	"context"
	"path/filepath"

	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/util"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/webhook"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/proto/mounter"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

type MounterServer struct {
	mounter.UnimplementedMounterServer
	ctx     context.Context
	Mounter *Mounter
}

func NewMounterServer(ctx context.Context, mounter *Mounter) *MounterServer {
	return &MounterServer{
		Mounter: mounter,
		ctx:     ctx,
	}
}

func (ms *MounterServer) Mount(ctx context.Context, req *mounter.MountRequest) (*mounter.MountResponse, error) {
	if req == nil {
		return nil, status.Error(codes.Internal, "mount request cannot be nil")
	}
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.Internal, "volume id cannot be empty")
	}
	if req.GetMountPoint() == "" {
		return nil, status.Error(codes.Internal, "mount point cannot be empty")
	}

	mc := MountConfig{
		VolumeName:       req.GetVolumeId(), // Set VolumeName to VolumeId for logging purposes.
		BucketName:       util.ParseVolumeID(req.GetVolumeId()),
		Options:          req.GetMountOptions(),
		SharedMountPoint: req.GetMountPoint(),
		TempDir:          webhook.SidecarContainerTmpVolumeMountPath,
		ErrWriter:        NewErrorWriter(filepath.Join(webhook.SidecarContainerTmpVolumeMountPath, util.ErrorFileName)),
		BufferDir:        webhook.SidecarContainerBufferVolumeMountPath,
		CacheDir:         webhook.SidecarContainerCacheVolumeMountPath,
		ConfigFile:       filepath.Join(webhook.SidecarContainerTmpVolumeMountPath, "config.yaml"),
		// TODO(FUECHR): Implement Auto Go Mem Limit.
	}

	// TODO(FUECHR): Implement Cloud Profiler hook (profiler.Start).
	// TODO(FUECHR): Implement ErrorWriter.
	// TODO(FUECHR): Implement defaultingFlagFileParsing.

	// TODO(FUECHR): Make prometheusPort increment inside prepareMountArgs thread-safe.
	mc.prepareMountArgs()
	// TODO(FUECHR): Call mergeFlags(mc.ConfigFileFlagMap, flagMapFromDriver).

	// TODO(FUECHR): Implement SetupTokenAndStorageManager for sidecar bucket access check.

	if err := mc.prepareConfigFile(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to prepare config file: %v", err)
	}

	klog.Infof("Start mounting bucket %q to %q for volume %q", mc.BucketName, mc.SharedMountPoint, mc.VolumeName)

	// TODO(FUECHR): Implement Cleaning of existing errors in ErrorWriter before mounting.

	// Use the mounter servers long running ctx to prevent the one from NodeStageVolume from killing the gcsfuse process.
	if err := ms.Mounter.MountToNode(ctx, ms.ctx, &mc); err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		return nil, status.Errorf(codes.Internal, "failed to mount bucket %q for volume %q: %v", mc.BucketName, mc.VolumeName, err)
	}
	return &mounter.MountResponse{}, nil
}
