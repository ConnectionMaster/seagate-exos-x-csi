package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	storageapi "github.com/Seagate/seagate-exos-x-api-go"
	"github.com/Seagate/seagate-exos-x-csi/pkg/common"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

func getVolumeMapsHostNames(client *storageapi.Client, name string) ([]string, *storageapi.ResponseStatus, error) {
	if name != "" {
		name = fmt.Sprintf("\"%s\"", name)
	}
	res, status, err := client.Request(fmt.Sprintf("/show/volume-maps/%s", name))
	if err != nil {
		return []string{}, status, err
	}

	hostNames := []string{}
	for _, rootObj := range res.Objects {
		if rootObj.Name != "volume-view" {
			continue
		}

		for _, object := range rootObj.Objects {
			hostName := object.PropertiesMap["identifier"].Data
			if object.Name == "host-view" && hostName != "all other hosts" {
				hostNames = append(hostNames, hostName)
			}
		}
	}

	return hostNames, status, err
}

// ControllerPublishVolume attaches the given volume to the node
func (driver *Controller) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume with empty ID")
	}
	if len(req.GetNodeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume to a node with empty ID")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume without capabilities")
	}

	initiatorName := req.GetNodeId()
	klog.Infof("attach request for initiator %s, volume id: %s", initiatorName, req.GetVolumeId())

	hostNames, _, err := getVolumeMapsHostNames(driver.client, req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	for _, hostName := range hostNames {
		if hostName != initiatorName {
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s is already attached to another node", req.GetVolumeId())
		}
	}

	lun, err := driver.chooseLUN(initiatorName)
	if err != nil {
		return nil, err
	}
	klog.Infof("using LUN %d", lun)

	if err = driver.mapVolume(req.GetVolumeId(), initiatorName, lun); err != nil {
		return nil, err
	}

	klog.Infof("successfully mapped volume %s for initiator %s", req.GetVolumeId(), initiatorName)
	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{"lun": strconv.Itoa(lun)},
	}, nil
}

// ControllerUnpublishVolume deattaches the given volume from the node
func (driver *Controller) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot unpublish volume with empty ID")
	}

	klog.Infof("unmapping volume %s from initiator %s", req.GetVolumeId(), req.GetNodeId())
	_, status, err := driver.client.UnmapVolume(req.GetVolumeId(), req.GetNodeId())
	if err != nil {
		if status != nil && status.ReturnCode == unmapFailedErrorCode {
			klog.Info("unmap failed, assuming volume is already unmapped")
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}

		return nil, err
	}

	klog.Infof("successfully unmapped volume %s from all initiators", req.GetVolumeId())
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (driver *Controller) chooseLUN(initiatorName string) (int, error) {
	klog.Infof("listing all LUN mappings")
	volumes, responseStatus, err := driver.client.ShowHostMaps(initiatorName)
	if err != nil && responseStatus == nil {
		return -1, err
	}
	if responseStatus.ReturnCode == hostMapDoesNotExistsErrorCode {
		klog.Info("initiator does not exist, assuming there is no LUN mappings yet and using LUN 1")
		return 1, nil
	}
	if err != nil {
		return -1, err
	}

	sort.Sort(Volumes(volumes))

	klog.V(5).Infof("checking if LUN 1 is not already in use")
	if len(volumes) == 0 || volumes[0].LUN > 1 {
		return 1, nil
	}

	klog.V(5).Infof("searching for an available LUN between LUNs in use")
	for index := 1; index < len(volumes); index++ {
		if volumes[index].LUN-volumes[index-1].LUN > 1 {
			return volumes[index-1].LUN + 1, nil
		}
	}

	klog.V(5).Infof("checking if next LUN is not above maximum LUNs limit")
	if volumes[len(volumes)-1].LUN+1 < common.MaximumLUN {
		return volumes[len(volumes)-1].LUN + 1, nil
	}

	return -1, status.Error(codes.ResourceExhausted, "no more available LUNs")
}

func (driver *Controller) mapVolume(volumeName, initiatorName string, lun int) error {
	klog.Infof("trying to map volume %s for initiator %s on LUN %d", volumeName, initiatorName, lun)
	_, metadata, err := driver.client.MapVolume(volumeName, initiatorName, "rw", lun)
	if err != nil && metadata == nil {
		return err
	}
	if metadata.ReturnCode == hostDoesNotExistsErrorCode {
		nodeIDParts := strings.Split(initiatorName, ":")
		if len(nodeIDParts) < 2 {
			return status.Error(codes.InvalidArgument, "specified node ID is not a valid IQN")
		}

		nodeName := strings.Join(nodeIDParts[1:], ":")
		klog.Infof("initiator does not exist, creating it with nickname %s", nodeName)
		_, _, err = driver.client.CreateHost(nodeName, initiatorName)
		if err != nil {
			return err
		}
		klog.Info("retrying to map volume")
		_, _, err = driver.client.MapVolume(volumeName, initiatorName, "rw", lun)
		if err != nil {
			return err
		}
	} else if metadata.ReturnCode == volumeNotFoundErrorCode {
		return status.Errorf(codes.NotFound, "volume %s not found", volumeName)
	} else if err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	return nil
}
