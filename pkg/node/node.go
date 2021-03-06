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

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/enix/dothill-csi/pkg/common"
	"github.com/kubernetes-csi/csi-lib-iscsi/iscsi"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

// Driver is the implementation of csi.NodeServer
type Driver struct {
	semaphore   *semaphore.Weighted
	kubeletPath string
}

// NewDriver is a convenience function for creating a node driver
func NewDriver(kubeletPath string) *Driver {
	if klog.V(8) {
		iscsi.EnableDebugLogging(os.Stderr)
	}

	return &Driver{
		semaphore:   semaphore.NewWeighted(1),
		kubeletPath: kubeletPath,
	}
}

// NewServerInterceptors implements DriverImpl.NewServerInterceptors
func (driver *Driver) NewServerInterceptors(logRoutineServerInterceptor grpc.UnaryServerInterceptor) *[]grpc.UnaryServerInterceptor {
	serverInterceptors := []grpc.UnaryServerInterceptor{
		func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			if info.FullMethod == "/csi.v1.Node/NodePublishVolume" {
				if !driver.semaphore.TryAcquire(1) {
					return nil, status.Error(codes.Aborted, "node busy: too many concurrent volume publication, try again later")
				}
				defer driver.semaphore.Release(1)
			}
			return handler(ctx, req)
		},
		logRoutineServerInterceptor,
	}

	return &serverInterceptors
}

// ShouldLogRoutine implements DriverImpl.ShouldLogRoutine
func (driver *Driver) ShouldLogRoutine(fullMethod string) bool {
	return fullMethod == "/csi.v1.Node/NodePublishVolume" ||
		fullMethod == "/csi.v1.Node/NodeUnpublishVolume" ||
		fullMethod == "/csi.v1.Node/NodeExpandVolume"
}

// NodeGetInfo returns info about the node
func (driver *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
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
func (driver *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
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
func (driver *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
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

	portals := strings.Split(req.GetVolumeContext()[common.PortalsConfigKey], ",")
	klog.Infof("ISCSI portals: %s", portals)

	lun, _ := strconv.ParseInt(req.GetPublishContext()["lun"], 10, 32)
	klog.Infof("LUN: %d", lun)

	klog.Info("initiating ISCSI connection...")
	targets := make([]iscsi.TargetInfo, 0)
	for _, portal := range portals {
		targets = append(targets, iscsi.TargetInfo{
			Iqn:    req.GetVolumeContext()[common.TargetIQNConfigKey],
			Portal: portal,
		})
	}
	connector := &iscsi.Connector{
		Targets:     targets,
		Lun:         int32(lun),
		DoDiscovery: true,
	}
	path, err := connector.Connect()
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	klog.Infof("attached device at %s", path)

	if connector.IsMultipathEnabled() {
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
		return nil, status.Errorf(codes.DataLoss, "Filesystem seems to be corrupted: %v", err)
	}

	klog.Infof("mounting volume at %s", req.GetTargetPath())
	os.Mkdir(req.GetTargetPath(), 00755)
	out, err := exec.Command("mount", "-t", fsType, path, req.GetTargetPath()).CombinedOutput()
	if err != nil {
		return nil, status.Error(codes.Internal, string(out))
	}

	iscsiInfoPath := driver.getIscsiInfoPath(req.GetVolumeId())
	klog.Infof("saving ISCSI connection info in %s", iscsiInfoPath)
	err = connector.Persist(iscsiInfoPath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.Infof("successfully mounted volume at %s", req.GetTargetPath())
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts the volume from the target path
func (driver *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot unpublish volume with empty id")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume at an empty path")
	}

	klog.Infof("unpublishing volume %s", req.GetVolumeId())

	_, err := os.Stat(req.GetTargetPath())
	if err == nil {
		klog.Infof("unmounting volume at %s", req.GetTargetPath())
		out, err := exec.Command("mountpoint", req.GetTargetPath()).CombinedOutput()
		if err == nil {
			out, err := exec.Command("umount", req.GetTargetPath()).CombinedOutput()
			if err != nil && !os.IsNotExist(err) {
				return nil, status.Error(codes.Internal, string(out))
			}
		} else {
			klog.Warningf("assuming that volume is already unmounted: %s", out)
		}

		os.Remove(req.GetTargetPath())
	}

	iscsiInfoPath := driver.getIscsiInfoPath(req.GetVolumeId())
	klog.Infof("loading ISCSI connection info from %s", iscsiInfoPath)
	connector, err := iscsi.GetConnectorFromFile(iscsiInfoPath)
	if err != nil {
		klog.Warning(errors.Wrap(err, "assuming that ISCSI connection is already closed"))
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	if isVolumeInUse(connector.MountTargetDevice.GetPath()) {
		klog.Info("volume is still in use on the node, thus it will not be detached")
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	if err = checkFs(connector.MountTargetDevice.GetPath()); err != nil {
		return nil, status.Errorf(codes.DataLoss, "Filesystem seems to be corrupted: %v", err)
	}

	klog.Info("detaching ISCSI device")
	err = connector.DisconnectVolume()
	if err != nil {
		return nil, err
	}

	klog.Infof("deleting ISCSI connection info file %s", iscsiInfoPath)
	os.Remove(iscsiInfoPath)

	klog.Info("successfully detached ISCSI device")
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeExpandVolume finalizes volume expansion on the node
func (driver *Driver) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	iscsiInfoPath := driver.getIscsiInfoPath(req.GetVolumeId())
	connector, err := iscsi.GetConnectorFromFile(iscsiInfoPath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	for i := range connector.Devices {
		connector.Devices[i].Rescan()
	}

	if connector.IsMultipathEnabled() {
		klog.V(2).Info("device is using multipath")
		if err := iscsi.ResizeMultipathDevice(connector.MountTargetDevice); err != nil {
			return nil, err
		}
	} else {
		klog.V(2).Info("device is NOT using multipath")
	}

	klog.Infof("expanding filesystem on device %s", connector.MountTargetDevice.GetPath())
	output, err := exec.Command("resize2fs", connector.MountTargetDevice.GetPath()).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("could not resize filesystem: %v", output)
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

// NodeGetVolumeStats return info about a given volume
// Will not be called as the plugin does not have the GET_VOLUME_STATS capability
func (driver *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeGetVolumeStats is unimplemented and should not be called")
}

// NodeStageVolume mounts the volume to a staging path on the node. This is
// called by the CO before NodePublishVolume and is used to temporary mount the
// volume to a staging path. Once mounted, NodePublishVolume will make sure to
// mount it to the appropriate path
// Will not be called as the plugin does not have the STAGE_UNSTAGE_VOLUME capability
func (driver *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeStageVolume is unimplemented and should not be called")
}

// NodeUnstageVolume unstages the volume from the staging path
// Will not be called as the plugin does not have the STAGE_UNSTAGE_VOLUME capability
func (driver *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeUnstageVolume is unimplemented and should not be called")
}

// Probe returns the health and readiness of the plugin
func (driver *Driver) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	if !isKernelModLoaded("iscsi_tcp") {
		return nil, status.Error(codes.FailedPrecondition, "kernel mod iscsi_tcp is not loaded")
	}
	if !isKernelModLoaded("dm_multipath") {
		return nil, status.Error(codes.FailedPrecondition, "kernel mod dm_multipath is not loaded")
	}

	return &csi.ProbeResponse{}, nil
}

func (driver *Driver) getIscsiInfoPath(volumeID string) string {
	return fmt.Sprintf("%s/plugins/%s/iscsi-%s.json", driver.kubeletPath, common.PluginName, volumeID)
}

func isKernelModLoaded(modName string) bool {
	klog.V(5).Infof("verifiying that %q kernel mod is loaded", modName)
	err := exec.Command("grep", "^"+modName, "/proc/modules", "-q").Run()

	if err != nil {
		return false
	}

	klog.V(5).Infof("kernel mod %q is loaded", modName)

	return true
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
	output, err := exec.Command("blkid",
		"--probe",
		"--match-tag", "TYPE",
		"--match-tag", "PTTYPE",
		"--output", "export",
		device).CombinedOutput()

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

func isVolumeInUse(devicePath string) bool {
	_, err := exec.Command("findmnt", devicePath).CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false
		}
	}
	return true
}
