package node

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Seagate/csi-lib-iscsi/iscsi"
	"github.com/Seagate/seagate-exos-x-csi/pkg/common"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

// Node is the implementation of csi.NodeServer
type Node struct {
	*common.Driver

	semaphore *semaphore.Weighted
	runPath   string
}

// New is a convenience function for creating a node driver
func New() *Node {
	if klog.V(8) {
		iscsi.EnableDebugLogging(os.Stderr)
	}

	node := &Node{
		Driver:    common.NewDriver(),
		semaphore: semaphore.NewWeighted(1),
		runPath:   fmt.Sprintf("/var/run/%s", common.PluginName),
	}

	if err := os.MkdirAll(node.runPath, 0755); err != nil {
		panic(err)
	}

	klog.Infof("Node initializing with path: %s", node.runPath)

	requiredBinaries := []string{
		"blkid",      // command-line utility to locate/print block device attributes
		"findmnt",    // find a filesystem
		"iscsiadm",   // iscsi administration
		"mount",      // mount a filesystem
		"mountpoint", // see if a directory or file is a mountpoint
		"multipath",  // device mapping multipathing
		"multipathd", // device mapping multipathing
		"umount",     // unmount file systems

		// "blockdev",    // call block device ioctls from the command line
		// "lsblk",       // list block devices
		// "scsi_id",     // retrieve and generate a unique SCSI identifier
		//	"e2fsck",     // check a Linux ext2/ext3/ext4 file system
		//	"mkfs.ext4",  // create an ext2/ext3/ext4 filesystem
		//	"resize2fs",  // ext2/ext3/ext4 file system resizer
	}

	klog.Infof("Checking (%d) binaries", len(requiredBinaries))

	for _, binaryName := range requiredBinaries {
		if err := checkHostBinary(binaryName); err != nil {
			klog.Warningf("Error locating binary %q", binaryName)
		}
	}

	node.InitServer(
		func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			if info.FullMethod == "/csi.v1.Node/NodePublishVolume" {
				if !node.semaphore.TryAcquire(1) {
					return nil, status.Error(codes.Aborted, "node busy: too many concurrent volume publication, try again later")
				}
				defer node.semaphore.Release(1)
			}
			return handler(ctx, req)
		},
		common.NewLogRoutineServerInterceptor(func(fullMethod string) bool {
			return fullMethod == "/csi.v1.Node/NodePublishVolume" ||
				fullMethod == "/csi.v1.Node/NodeUnpublishVolume" ||
				fullMethod == "/csi.v1.Node/NodeExpandVolume"
		}),
	)

	csi.RegisterIdentityServer(node.Server, node)
	csi.RegisterNodeServer(node.Server, node)

	return node
}

// NodeGetInfo returns info about the node
func (node *Node) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	initiatorName, err := readInitiatorName()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	return &csi.NodeGetInfoResponse{
		NodeId:            initiatorName,
		MaxVolumesPerNode: 255,
	}, nil
}

// NodeGetCapabilities returns the supported capabilities of the node server
func (node *Node) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	var csc []*csi.NodeServiceCapability
	cl := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
	}

	for _, cap := range cl {
		klog.V(4).Infof("enabled node service capability: %v", cap.String())
		csc = append(csc, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		})
	}

	return &csi.NodeGetCapabilitiesResponse{Capabilities: csc}, nil
}

// NodePublishVolume mounts the volume mounted to the staging path to the target path
func (node *Node) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume with empty id")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume at an empty path")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume without capabilities")
	}

	klog.Infof("publishing volume %s", req.GetVolumeId())

	iqn := req.GetVolumeContext()["iqn"]
	portals := strings.Split(req.GetVolumeContext()["portals"], ",")
	klog.Infof("iSCSI iqn: %s, portals: %v", iqn, portals)

	lun, _ := strconv.ParseInt(req.GetPublishContext()["lun"], 10, 32)
	klog.Infof("LUN: %d", lun)

	klog.Info("initiating ISCSI connection...")
	targets := make([]iscsi.TargetInfo, 0)
	for _, portal := range portals {
		targets = append(targets, iscsi.TargetInfo{
			Iqn:    iqn,
			Portal: portal,
		})
	}
	connector := iscsi.Connector{
		Targets:     targets,
		Lun:         int32(lun),
		DoDiscovery: true,
	}

	path, err := iscsi.Connect(&connector)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	klog.Infof("attached device at %s", path)

	if connector.Multipath {
		klog.Info("device is using multipath")
	} else {
		klog.Info("device is NOT using multipath")
	}

	fsType := req.GetVolumeContext()[common.FsTypeConfigKey]
	err = ensureFsType(fsType, path)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err = checkFs(path); err != nil {
		return nil, status.Errorf(codes.DataLoss, "filesystem seems to be corrupted: %v", err)
	}

	out, err := exec.Command("findmnt", "--output", "TARGET", "--noheadings", path).Output()
	mountpoints := strings.Split(strings.Trim(string(out), "\n"), "\n")
	if err != nil || len(mountpoints) == 0 {
		klog.Infof("mounting volume at %s", req.GetTargetPath())
		os.Mkdir(req.GetTargetPath(), 00755)
		out, err = exec.Command("mount", "-t", fsType, path, req.GetTargetPath()).CombinedOutput()
		if err != nil {
			return nil, status.Error(codes.Internal, string(out))
		}
	} else if len(mountpoints) == 1 {
		if mountpoints[0] == req.GetTargetPath() {
			klog.Infof("volume %s already mounted", req.GetTargetPath())
		} else {
			errStr := fmt.Sprintf("device has already been mounted somewhere else (%s instead of %s), please unmount first", mountpoints[0], req.GetTargetPath())
			return nil, status.Error(codes.Internal, errStr)
		}
	} else if len(mountpoints) > 1 {
		return nil, errors.New("device has already been mounted in several locations, please unmount first")
	}

	iscsiInfoPath := node.getIscsiInfoPath(req.GetVolumeId())
	klog.Infof("saving ISCSI connection info in %s", iscsiInfoPath)
	err = iscsi.PersistConnector(&connector, iscsiInfoPath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.Infof("successfully mounted volume at %s", req.GetTargetPath())
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts the volume from the target path
func (node *Node) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot unpublish volume with an empty volume id")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot unpublish volume with an empty target path")
	}

	klog.Infof("unpublishing volume %s at target path %s", req.GetVolumeId(), req.GetTargetPath())

	_, err := os.Stat(req.GetTargetPath())
	if err == nil {
		klog.Infof("unmounting volume at %s", req.GetTargetPath())
		out, err := exec.Command("mountpoint", req.GetTargetPath()).CombinedOutput()
		if err == nil {
			out, err := exec.Command("umount", req.GetTargetPath()).CombinedOutput()
			if err != nil {
				return nil, status.Error(codes.Internal, string(out))
			}
		} else {
			klog.Warningf("assuming that volume is already unmounted: %s", out)
		}

		err = os.Remove(req.GetTargetPath())
		if err != nil && !os.IsNotExist(err) {
			return nil, status.Error(codes.Internal, err.Error())
		}
	} else {
		klog.Warningf("assuming that volume is already unmounted: %v", err)
	}

	iscsiInfoPath := node.getIscsiInfoPath(req.GetVolumeId())
	klog.Infof("loading ISCSI connection info from %s", iscsiInfoPath)
	connector, err := iscsi.GetConnectorFromFile(iscsiInfoPath)
	if err != nil {
		if os.IsNotExist(err) {
			klog.Warning(errors.Wrap(err, "assuming that ISCSI connection is already closed"))
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	klog.Infof("connector.DevicePath (%s)", connector.DevicePath)

	if isVolumeInUse(connector.DevicePath) {
		klog.Info("volume is still in use on the node, thus it will not be detached")
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	_, err = os.Stat(connector.DevicePath)
	if err != nil && os.IsNotExist(err) {
		klog.Warningf("assuming that volume is already disconnected: %s", err)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	if err = checkFs(connector.DevicePath); err != nil {
		return nil, status.Errorf(codes.DataLoss, "Filesystem seems to be corrupted: %v", err)
	}

	klog.Info("DisconnectVolume, detaching ISCSI device")
	err = iscsi.DisconnectVolume(*connector)
	if err != nil {
		return nil, err
	}

	klog.Infof("deleting ISCSI connection info file %s", iscsiInfoPath)
	os.Remove(iscsiInfoPath)

	klog.Info("successfully detached ISCSI device")
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeExpandVolume finalizes volume expansion on the node
func (node *Node) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {

	volumeid := req.GetVolumeId()
	if len(volumeid) == 0 {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("node expand volume requires volume id"))
	}

	volumepath := req.GetVolumePath()
	if len(volumepath) == 0 {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("node expand volume requires volume path"))
	}

	iscsiInfoPath := node.getIscsiInfoPath(volumeid)
	connector, err := iscsi.GetConnectorFromFile(iscsiInfoPath)
	klog.V(3).Infof("GetConnectorFromFile(%s) connector: %v, err: %v", volumeid, connector, err)

	if err != nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("node expand volume path not found for volume id (%s)", volumeid))
	}

	// TODO: Is a rescan needed - rescan a scsi device by writing 1 in /sys/class/scsi_device/h:c:t:l/device/rescan
	// for i := range connector.Devices {
	// 	connector.Devices[i].Rescan()
	// }

	if connector.Multipath {
		klog.V(2).Info("device is using multipath")
		if err := iscsi.ResizeMultipathDevice(connector.DevicePath); err != nil {
			return nil, err
		}
	} else {
		klog.V(2).Info("device is NOT using multipath")
	}

	klog.Infof("expanding filesystem on device %s", connector.DevicePath)
	output, err := exec.Command("resize2fs", connector.DevicePath).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("could not resize filesystem: %v", output)
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

// NodeGetVolumeStats return info about a given volume
// Will not be called as the plugin does not have the GET_VOLUME_STATS capability
func (node *Node) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeGetVolumeStats is unimplemented and should not be called")
}

// NodeStageVolume mounts the volume to a staging path on the node. This is
// called by the CO before NodePublishVolume and is used to temporary mount the
// volume to a staging path. Once mounted, NodePublishVolume will make sure to
// mount it to the appropriate path
// Will not be called as the plugin does not have the STAGE_UNSTAGE_VOLUME capability
func (node *Node) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeStageVolume is unimplemented and should not be called")
}

// NodeUnstageVolume unstages the volume from the staging path
// Will not be called as the plugin does not have the STAGE_UNSTAGE_VOLUME capability
func (node *Node) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeUnstageVolume is unimplemented and should not be called")
}

// Probe returns the health and readiness of the plugin
func (node *Node) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	klog.V(4).Infof("Probe called with args: %#v", req)
	return &csi.ProbeResponse{Ready: &wrappers.BoolValue{Value: true}}, nil
}

func (node *Node) getIscsiInfoPath(volumeID string) string {
	return fmt.Sprintf("%s/iscsi-%s.json", node.runPath, volumeID)
}

func checkHostBinary(name string) error {
	klog.V(5).Infof("checking that binary %q exists in host PATH", name)

	if path, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("binary %q not found", name)
	} else {
		klog.V(5).Infof("found binary %q in host PATH at %q", name, path)
	}

	return nil
}

func checkFs(path string) error {
	klog.Infof("Checking filesystem at %s", path)
	if out, err := exec.Command("e2fsck", "-n", path).CombinedOutput(); err != nil {
		return errors.New(string(out))
	}
	return nil
}

func findDeviceFormat(device string) (string, error) {
	klog.V(2).Infof("Trying to find filesystem format on device %q", device)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "blkid",
		"-p",
		"-s", "TYPE",
		"-s", "PTTYPE",
		"-o", "export",
		device).CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		err = errors.New("command timed out after 2 seconds")
	}

	klog.V(2).Infof("blkid output: %q,", output)

	if err != nil {
		// blkid exit with code 2 if the specified token (TYPE/PTTYPE, etc) could not be found or if device could not be identified.
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 2 {
			klog.V(2).Infof("Device seems to be is unformatted (%v)", err)
			return "", nil
		}
		return "", fmt.Errorf("could not not find format for device %q (%v)", device, err)
	}

	re := regexp.MustCompile(`([A-Z]+)="?([^"\n]+)"?`) // Handles alpine and debian outputs
	matches := re.FindAllSubmatch(output, -1)

	var filesystemType, partitionType string
	for _, match := range matches {
		if len(match) != 3 {
			return "", fmt.Errorf("invalid blkid output: %s", output)
		}
		key := string(match[1])
		value := string(match[2])

		if key == "TYPE" {
			filesystemType = value
		} else if key == "PTTYPE" {
			partitionType = value
		}
	}

	if partitionType != "" {
		klog.V(2).Infof("Device %q seems to have a partition table type: %s", partitionType)
		return "OTHER/PARTITIONS", nil
	}

	return filesystemType, nil
}

func ensureFsType(fsType string, disk string) error {
	currentFsType, err := findDeviceFormat(disk)
	if err != nil {
		return err
	}

	klog.V(1).Infof("Detected filesystem: %q", currentFsType)
	if currentFsType != fsType {
		if currentFsType != "" {
			return fmt.Errorf("Could not create %s filesystem on device %s since it already has one (%s)", fsType, disk, currentFsType)
		}

		klog.Infof("Creating %s filesystem on device %s", fsType, disk)
		out, err := exec.Command(fmt.Sprintf("mkfs.%s", fsType), disk).CombinedOutput()
		if err != nil {
			return errors.New(string(out))
		}
	}

	return nil
}

func readInitiatorName() (string, error) {
	initiatorNameFilePath := "/etc/iscsi/initiatorname.iscsi"
	file, err := os.Open(initiatorNameFilePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if equal := strings.Index(line, "="); equal >= 0 {
			if strings.TrimSpace(line[:equal]) == "InitiatorName" {
				return strings.TrimSpace(line[equal+1:]), nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}

	return "", fmt.Errorf("InitiatorName key is missing from %s", initiatorNameFilePath)
}

// isVolumeInUse: Use findmnt to determine if the devie path is mounted or not.
func isVolumeInUse(devicePath string) bool {
	_, err := exec.Command("findmnt", devicePath).CombinedOutput()
	klog.Infof("isVolumeInUse: findmnt %s, err=%v", devicePath, err)
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false
		}
	}
	return true
}
