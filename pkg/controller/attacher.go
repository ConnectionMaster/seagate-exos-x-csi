package controller

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/enix/dothill-storage-controller/pkg/common"
	"k8s.io/klog"
)

// ControllerPublishVolume attaches the given volume to the node
func (driver *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	err := driver.beginRoutine(&common.DriverCtx{
		Req:         req,
		Credentials: req.GetSecrets(),
	})
	defer driver.endRoutine()
	if err != nil {
		return nil, err
	}

	initiatorName := getInitiatorName(req.GetVolumeContext())
	klog.Infof("attach request for initiator %s, volume id : %s", initiatorName, req.GetVolumeId())

	lun, err := driver.chooseLUN()
	if err != nil {
		return nil, err
	}
	klog.Infof("using LUN %d", lun)

	err = driver.mapVolume(req.GetVolumeId(), initiatorName, lun)
	if err != nil {
		// klog.Infof("volume %s couldn't be mapped, deleting it", req.GetVolumeId())
		// driver.dothillClient.DeleteVolume(volumeName)
		return nil, err
	}

	klog.Infof("succesfully mapped volume %s for initiator %s", req.GetVolumeId(), initiatorName)
	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{"lun": strconv.Itoa(lun)},
	}, nil
}

// ControllerUnpublishVolume deattaches the given volume from the node
func (driver *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	err := driver.beginRoutine(&common.DriverCtx{
		Req:         req,
		Credentials: req.GetSecrets(),
	})
	defer driver.endRoutine()
	if err != nil {
		return nil, err
	}

	klog.Infof("unmapping volume %s from all initiators", req.GetVolumeId())
	_, status, err := driver.dothillClient.UnmapVolume(req.GetVolumeId(), "")
	if err != nil {
		if status.ReturnCode == unmapFailedErrorCode {
			klog.Info("unmap failed, assuming volume is already unmapped")
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}

		return nil, err
	}

	klog.Infof("successfully unmapped volume %s from all initiators", req.GetVolumeId())
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (driver *Driver) chooseLUN() (int, error) {
	klog.Infof("listing all LUN mappings")
	volumes, status, err := driver.dothillClient.ShowHostMaps("")
	if err != nil && status == nil {
		return -1, err
	}
	if status.ReturnCode == hostMapDoesNotExistsErrorCode {
		klog.Info("initiator does not exist, assuming there is no LUN mappings yet and using LUN 1")
		return 1, nil
	}
	if err != nil {
		return -1, err
	}

	sort.Sort(Volumes(volumes))
	index := 1
	for ; index < len(volumes); index++ {
		if volumes[index].LUN-volumes[index-1].LUN > 1 {
			return volumes[index-1].LUN + 1, nil
		}
	}

	if volumes[len(volumes)-1].LUN+1 < common.MaximumLUN {
		return volumes[len(volumes)-1].LUN + 1, nil
	}

	return -1, errors.New("no more available LUNs")
}

func (driver *Driver) mapVolume(volumeName, initiatorName string, lun int) error {
	klog.Infof("trying to map volume %s for initiator %s on LUN %d", volumeName, initiatorName, lun)
	_, status, err := driver.dothillClient.MapVolume(volumeName, initiatorName, "rw", lun)
	if err != nil && status == nil {
		return err
	}
	if status.ReturnCode == hostDoesNotExistsErrorCode {
		nodeName := strings.Split(initiatorName, ":")[1]
		klog.Infof("initiator does not exist, creating it with nickname %s", nodeName)
		_, _, err = driver.dothillClient.CreateHost(nodeName, initiatorName)
		if err != nil {
			return err
		}
		klog.Info("retrying to map volume")
		_, _, err = driver.dothillClient.MapVolume(volumeName, initiatorName, "rw", lun)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	return nil
}

func getInitiatorName(volumeContext map[string]string) string {
	initiatorName := volumeContext[common.InitiatorNameConfigKey]
	// overrideInitiatorName, overrideExists := options.PVC.Annotations[initiatorNameConfigKey]
	// if overrideExists {
	// 	initiatorName = overrideInitiatorName
	// 	klog.Infof("custom initiator name was specified in PVC annotation: %s", initiatorName)
	// } else if options.Parameters[uniqueInitiatorNameByPvcConfigKey] == "true" {
	// 	year, month, _ := time.Now().Date()
	// 	uniquePart := fmt.Sprintf("%d", rand.Int())[:8]
	// 	initiatorName = fmt.Sprintf("iqn.%d-%02d.local.cluster:%s", year, int(month), uniquePart)
	// 	klog.Infof("generated initiator name: %s", initiatorName)
	// }

	return initiatorName
}
