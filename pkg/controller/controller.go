package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"

	storageapi "github.com/Seagate/seagate-exos-x-api-go"
	"github.com/Seagate/seagate-exos-x-csi/pkg/common"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

const (
	snapshotNotFoundErrorCode             = -10050
	hostMapDoesNotExistsErrorCode         = -10074
	volumeNotFoundErrorCode               = -10075
	volumeHasSnapshot                     = -10183
	snapshotAlreadyExists                 = -10186
	initiatorNicknameOrIdentifierNotFound = -10386
	unmapFailedErrorCode                  = -10509
)

var volumeCapabilities = []*csi.VolumeCapability{
	{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	},
}

var csiMutexes = map[string]*sync.Mutex{
	"/csi.v1.Controller/CreateVolume":              {},
	"/csi.v1.Controller/ControllerPublishVolume":   {},
	"/csi.v1.Controller/DeleteVolume":              {},
	"/csi.v1.Controller/ControllerUnpublishVolume": {},
	"/csi.v1.Controller/ControllerExpandVolume":    {},
}

var nonAuthenticatedMethods = []string{
	"/csi.v1.Controller/ControllerGetCapabilities",
	"/csi.v1.Controller/ListVolumes",
	"/csi.v1.Controller/GetCapacity",
	"/csi.v1.Controller/ControllerGetVolume",
	"/csi.v1.Identity/Probe",
	"/csi.v1.Identity/GetPluginInfo",
	"/csi.v1.Identity/GetPluginCapabilities",
}

// Controller is the implementation of csi.ControllerServer
type Controller struct {
	*common.Driver

	client *storageapi.Client
}

// DriverCtx contains data common to most calls
type DriverCtx struct {
	Credentials map[string]string
	Parameters  map[string]string
	VolumeCaps  *[]*csi.VolumeCapability
}

// New is a convenience fn for creating a controller driver
func New() *Controller {
	client := storageapi.NewClient()
	controller := &Controller{
		Driver: common.NewDriver(client.Collector),
		client: client,
	}

	controller.InitServer(
		func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			if mutex, exists := csiMutexes[info.FullMethod]; exists {
				mutex.Lock()
				defer mutex.Unlock()
			}
			return handler(ctx, req)
		},
		common.NewLogRoutineServerInterceptor(func(string) bool {
			return true
		}),
		func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			driverContext := DriverCtx{}
			reqWithSecrets, ok := req.(common.WithSecrets)
			if ok {
				driverContext.Credentials = reqWithSecrets.GetSecrets()
			}
			if reqWithParameters, ok := req.(common.WithParameters); ok {
				driverContext.Parameters = reqWithParameters.GetParameters()
			}
			if reqWithVolumeCaps, ok := req.(common.WithVolumeCaps); ok {
				driverContext.VolumeCaps = reqWithVolumeCaps.GetVolumeCapabilities()
			}

			err := controller.beginRoutine(&driverContext, info.FullMethod)
			if err != nil {
				klog.Infof("controller.beginRoutine error for req = %x", reqWithSecrets)
			}
			defer controller.endRoutine()
			if err != nil {
				return nil, err
			}

			return handler(ctx, req)
		},
	)

	csi.RegisterIdentityServer(controller.Server, controller)
	csi.RegisterControllerServer(controller.Server, controller)

	return controller
}

// ControllerGetCapabilities returns the capabilities of the controller service.
func (controller *Controller) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	var csc []*csi.ControllerServiceCapability
	cl := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
	}

	for _, cap := range cl {
		klog.V(4).Infof("enabled controller service capability: %v", cap.String())
		csc = append(csc, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		})
	}

	return &csi.ControllerGetCapabilitiesResponse{Capabilities: csc}, nil
}

// ValidateVolumeCapabilities checks whether the volume capabilities requested
// are supported.
func (controller *Controller) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot validate volume with empty ID")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot validate volume without capabilities")
	}
	_, _, err := controller.client.ShowVolumes(volumeID)
	if err != nil {
		return nil, status.Error(codes.NotFound, "cannot validate volume not found")
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: volumeCapabilities,
		},
	}, nil
}

// ListVolumes returns a list of all requested volumes
func (controller *Controller) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListVolumes is unimplemented and should not be called")
}

// GetCapacity returns the capacity of the storage pool
func (controller *Controller) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "GetCapacity is unimplemented and should not be called")
}

// ControllerGetVolume fetch current information about a volume
func (controller *Controller) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerGetVolume is unimplemented and should not be called")
}

// Probe returns the health and readiness of the plugin
func (controller *Controller) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

func (controller *Controller) beginRoutine(ctx *DriverCtx, methodName string) error {
	if err := runPreflightChecks(ctx.Parameters, ctx.VolumeCaps); err != nil {
		return err
	}

	needsAuthentication := true
	for _, name := range nonAuthenticatedMethods {
		if methodName == name {
			needsAuthentication = false
			break
		}
	}

	if !needsAuthentication {
		return nil
	}

	if ctx.Credentials == nil {
		return errors.New("missing API credentials")
	}

	return controller.configureClient(ctx.Credentials)
}

func (controller *Controller) endRoutine() {
	controller.client.HTTPClient.CloseIdleConnections()
}

func (controller *Controller) configureClient(credentials map[string]string) error {
	username := string(credentials[common.UsernameSecretKey])
	password := string(credentials[common.PasswordSecretKey])
	apiAddr := string(credentials[common.APIAddressConfigKey])

	if len(username) == 0 {
		return status.Error(codes.InvalidArgument, fmt.Sprintf("(%s) is missing from secrets", common.UsernameSecretKey))
	}

	if len(password) == 0 {
		return status.Error(codes.InvalidArgument, fmt.Sprintf("(%s) is missing from secrets", common.PasswordSecretKey))
	}

	if len(apiAddr) == 0 {
		return status.Error(codes.InvalidArgument, fmt.Sprintf("(%s) is missing from secrets", common.APIAddressConfigKey))
	}

	klog.Infof("using API at address (%s)", apiAddr)
	if controller.client.SessionValid(apiAddr, username) {
		return nil
	}

	controller.client.Username = username
	controller.client.Password = password
	controller.client.Addr = apiAddr
	klog.Infof("login to API address %q as user %q", controller.client.Addr, controller.client.Username)
	err := controller.client.Login()
	if err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}

	klog.Info("login was successful")
	err = controller.client.InitSystemInfo()

	return err
}

func runPreflightChecks(parameters map[string]string, capabilities *[]*csi.VolumeCapability) error {
	checkIfKeyExistsInConfig := func(key string) error {
		if parameters == nil {
			return nil
		}

		klog.V(2).Infof("checking for %s in storage class parameters", key)
		_, ok := parameters[key]
		if !ok {
			return status.Errorf(codes.InvalidArgument, "'%s' is missing from configuration", key)
		}
		return nil
	}

	if err := checkIfKeyExistsInConfig(common.FsTypeConfigKey); err != nil {
		return err
	}
	if err := checkIfKeyExistsInConfig(common.PoolConfigKey); err != nil {
		return err
	}

	if capabilities != nil {
		if len(*capabilities) == 0 {
			return status.Error(codes.InvalidArgument, "missing volume capabilities")
		}
		for _, capability := range *capabilities {
			if capability.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
				return status.Error(codes.FailedPrecondition, "storage only supports ReadWriteOnce access mode")
			}
		}
	}

	return nil
}
