package common

import (
	"context"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Seagate/seagate-exos-x-csi/pkg/exporter"
	"github.com/container-storage-interface/spec/lib/go/csi"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"k8s.io/klog"
)

// PluginName is the public name to be used in storage class etc.
const PluginName = "csi-exos-x.seagate.com"

// Configuration constants
const (
	FsTypeConfigKey           = "fsType"
	PoolConfigKey             = "pool"
	APIAddressConfigKey       = "apiAddress"
	UsernameSecretKey         = "username"
	PasswordSecretKey         = "password"
	StorageClassAnnotationKey = "storageClass"
	VolumePrefixKey           = "volPrefix"

	MaximumLUN            = 255
	VolumeNameMaxLength   = 32
	VolumePrefixMaxLength = 3
)

// Driver contains main resources needed by the driver and references the underlying specific driver
type Driver struct {
	Server *grpc.Server

	socket   net.Listener
	exporter *exporter.Exporter
}

// WithSecrets is an interface for structs with secrets
type WithSecrets interface {
	GetSecrets() map[string]string
}

// WithParameters is an interface for structs with parameters
type WithParameters interface {
	GetParameters() map[string]string
}

// WithVolumeCaps is an interface for structs with volume capabilities
type WithVolumeCaps interface {
	GetVolumeCapabilities() *[]*csi.VolumeCapability
}

// NewDriver is a convenience function for creating an abstract driver
func NewDriver(collectors ...prometheus.Collector) *Driver {
	exporter := exporter.New(9842)

	for _, collector := range collectors {
		exporter.RegisterCollector(collector)
	}

	return &Driver{exporter: exporter}
}

func (driver *Driver) InitServer(unaryServerInterceptors ...grpc.UnaryServerInterceptor) {
	interceptors := append([]grpc.UnaryServerInterceptor{
		func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			start := time.Now()
			resp, err := handler(ctx, req)
			driver.exporter.Collector.IncCSIRPCCall(info.FullMethod, err == nil)
			driver.exporter.Collector.AddCSIRPCCallDuration(info.FullMethod, time.Since(start))
			return resp, err
		},
	}, unaryServerInterceptors...)

	driver.Server = grpc.NewServer(
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(interceptors...)),
	)
}

func NewLogRoutineServerInterceptor(shouldLogRoutine func(string) bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if shouldLogRoutine(info.FullMethod) {
			klog.Infof("=== [ROUTINE START] %s ===", info.FullMethod)
			defer klog.Infof("=== [ROUTINE END] %s ===", info.FullMethod)
		}

		result, err := handler(ctx, req)
		if err != nil {
			klog.Error(err)
		}

		return result, err
	}
}

// Start does the boilerplate stuff for starting the driver
// it loads its configuration from cli flags
func (driver *Driver) Start(bind string) {

	var ll klog.Level = 0
	for i := 0; i < 10; i++ {
		if klog.V(klog.Level(i)) {
			ll = klog.Level(i)
		} else {
			break
		}
	}

	klog.Infof("starting driver on %s (%s) [level %d]\n\n", runtime.GOOS, runtime.GOARCH, ll)

	parts := strings.Split(bind, "://")
	if len(parts) < 2 {
		klog.Fatal("please specify a protocol in your bind URI (e.g. \"tcp://\")")
	}

	if parts[0][:4] == "unix" {
		syscall.Unlink(parts[1])
	}
	socket, err := net.Listen(parts[0], parts[1])
	if err != nil {
		klog.Fatal(err)
	}
	driver.socket = socket

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	go func() {
		_ = <-sigc
		driver.Stop()
	}()

	go func() {
		driver.exporter.ListenAndServe()
	}()

	klog.Infof("driver listening on %s\n\n", bind)
	driver.Server.Serve(socket)
}

// Stop shuts down the driver
func (driver *Driver) Stop() {
	klog.Info("gracefully stopping...")
	driver.Server.GracefulStop()
	driver.socket.Close()
	driver.exporter.Shutdown()
}
