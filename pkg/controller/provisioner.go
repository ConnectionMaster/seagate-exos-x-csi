package controller

import (
	"context"
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/enix/dothill-storage-controller/pkg/common"
	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

// CreateVolume creates a new volume from the given request. The function is
// idempotent.
func (driver *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	driver.lock.Lock()
	defer driver.lock.Unlock()
	defer driver.dothillClient.HTTPClient.CloseIdleConnections()

	klog.V(9).Infof("CreateVolume() called with: %+v", req)
	parameters := req.GetParameters()
	size := req.GetCapacityRange().GetRequiredBytes()
	sizeStr := fmt.Sprintf("%diB", size)
	klog.Infof("received %s volume request\n", sizeStr)

	err := runPreflightChecks(parameters, req.GetVolumeCapabilities())
	if err != nil {
		return nil, err
	}

	err = driver.configureClient(req.GetSecrets(), parameters[common.APIAddressConfigKey])
	if err != nil {
		return nil, err
	}

	volumeID := uuid.NewUUID().String()[:common.VolumeNameMaxLength]
	klog.Infof("creating volume %s (size %s) in pool %s", volumeID, sizeStr, parameters[common.PoolConfigKey])
	_, _, err = driver.dothillClient.CreateVolume(volumeID, sizeStr, parameters[common.PoolConfigKey])
	if err != nil {
		return nil, err
	}

	volume := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			VolumeContext: req.GetParameters(),
			CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
			ContentSource: req.GetVolumeContentSource(),
		},
	}

	klog.Infof("created volume %s (%s)", volumeID, sizeStr)
	klog.V(8).Infof("created volume %+v", volume)
	return volume, nil
}

// DeleteVolume deletes the given volume. The function is idempotent.
func (driver *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.Infof("deleting volume %s", req.GetVolumeId())
	// return &csi.DeleteVolumeResponse{}, nil

	_, _, err := driver.dothillClient.DeleteVolume(req.GetVolumeId())
	if err != nil {
		return nil, err
	}

	klog.Infof("successfully deleted volume %s", req.GetVolumeId())
	return &csi.DeleteVolumeResponse{}, nil
}

func runPreflightChecks(parameters map[string]string, capabilities []*csi.VolumeCapability) error {
	checkIfKeyExistsInConfig := func(key string) error {
		klog.V(2).Infof("checking for %s in storage class parameters", key)
		_, ok := parameters[key]
		if !ok {
			return status.Errorf(codes.FailedPrecondition, "'%s' is missing from configuration", key)
		}
		return nil
	}

	if err := checkIfKeyExistsInConfig(common.FsTypeConfigKey); err != nil {
		return err
	}
	if err := checkIfKeyExistsInConfig(common.PoolConfigKey); err != nil {
		return err
	}
	if err := checkIfKeyExistsInConfig(common.TargetIQNConfigKey); err != nil {
		return err
	}
	if err := checkIfKeyExistsInConfig(common.PortalsConfigKey); err != nil {
		return err
	}
	if err := checkIfKeyExistsInConfig(common.InitiatorNameConfigKey); err != nil {
		if err2 := checkIfKeyExistsInConfig(common.UniqueInitiatorNameByPvcConfigKey); err2 != nil {
			return errors.Wrap(err, err2.Error())
		}
	}
	if err := checkIfKeyExistsInConfig(common.APIAddressConfigKey); err != nil {
		return err
	}

	for _, capability := range capabilities {
		if capability.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return status.Error(codes.FailedPrecondition, "dothill storage only supports ReadWriteOnce access mode")
		}
	}

	return nil
}
