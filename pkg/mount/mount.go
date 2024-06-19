// Package mount provides utilities to detect,
// format and mount storage devices.
package mount

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	"k8s.io/utils/exec"
)

const (
	diskIDPath = "/dev/disk/by-id"
)

// Interface defines the set of methods to allow for
// mount operations on a system.
type Interface interface {
	mount.Interface
	exec.Interface

	FormatAndMount(source string, target string, fstype string, options []string) error

	CleanupMountPoint(path string, extensiveCheck bool) error
	GetDevicePath(ctx context.Context, volumeID string) (string, error)
	GetDeviceName(mountPath string) (string, int, error)
	ExistsPath(filename string) (bool, error)
	MakeDir(pathname string) error
	MakeFile(pathname string) error
	NewResizeFs(exec exec.Interface) *mount.ResizeFs
}

type mounter struct {
	mount.SafeFormatAndMount
	exec.Interface
}

// New creates an implementation of the mount.Interface.
func New() Interface {
	return &mounter{
		mount.SafeFormatAndMount{
			Interface: mount.New(""),
			Exec:      exec.New(),
		},
		exec.New(),
	}
}

func (m *mounter) CleanupMountPoint(path string, extensiveCheck bool) error {
	return mount.CleanupMountPoint(path, m, extensiveCheck)
}

func (m *mounter) GetDevicePath(ctx context.Context, volumeID string) (string, error) {
	backoff := wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   1.1,
		Steps:    15,
	}

	var devicePath string
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(context.Context) (bool, error) {
		path, err := m.getDevicePathBySerialID(volumeID)
		if err != nil {
			return false, err
		}
		if path != "" {
			devicePath = path

			return true, nil
		}
		m.probeVolume(ctx)

		return false, nil
	})

	if wait.Interrupted(err) {
		return "", fmt.Errorf("failed to find device for the volumeID: %q within the alloted time", volumeID)
	} else if devicePath == "" {
		return "", fmt.Errorf("device path was empty for volumeID: %q", volumeID)
	}

	return devicePath, nil
}

func (m *mounter) getDevicePathBySerialID(volumeID string) (string, error) {
	sourcePathPrefixes := []string{"virtio-", "scsi-", "scsi-0QEMU_QEMU_HARDDISK_"}
	serial := diskUUIDToSerial(volumeID)
	for _, prefix := range sourcePathPrefixes {
		source := filepath.Join(diskIDPath, prefix+serial)
		_, err := os.Stat(source)
		if err == nil {
			return source, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}

	return "", nil
}

func (m *mounter) probeVolume(ctx context.Context) {
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Scanning SCSI host")

	scsiPath := "/sys/class/scsi_host/"
	if dirs, err := os.ReadDir(scsiPath); err == nil {
		for _, f := range dirs {
			name := scsiPath + f.Name() + "/scan"
			data := []byte("- - -")
			logger.V(2).Info("Triggering SCSI host rescan")
			if err = os.WriteFile(name, data, 0o666); err != nil { //nolint:gosec
				logger.Error(err, "Failed to rescan scsi host ", "dirName", name)
			}
		}
	} else {
		logger.Error(err, "Failed to read dir ", "dirName", scsiPath)
	}

	args := []string{"trigger"}
	cmd := m.Exec.Command("udevadm", args...)
	_, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error(err, "Error running udevadm trigger")
	}
}

func (m *mounter) GetDeviceName(mountPath string) (string, int, error) {
	return mount.GetDeviceNameFromMount(m, mountPath)
}

// diskUUIDToSerial reproduces CloudStack function diskUuidToSerial
// from https://github.com/apache/cloudstack/blob/0f3f2a0937/plugins/hypervisors/kvm/src/main/java/com/cloud/hypervisor/kvm/resource/LibvirtComputingResource.java#L3000
//
// This is what CloudStack do *with KVM hypervisor* to translate
// a CloudStack volume UUID to libvirt disk serial.
func diskUUIDToSerial(uuid string) string {
	uuidWithoutHyphen := strings.ReplaceAll(uuid, "-", "")
	if len(uuidWithoutHyphen) < 20 {
		return uuidWithoutHyphen
	}

	return uuidWithoutHyphen[:20]
}

func (*mounter) ExistsPath(filename string) (bool, error) {
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return true, nil
}

func (*mounter) MakeDir(pathname string) error {
	err := os.MkdirAll(pathname, os.FileMode(0o755))
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}

	return nil
}

func (*mounter) MakeFile(pathname string) error {
	f, err := os.OpenFile(pathname, os.O_CREATE, os.FileMode(0o644))
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	if err = f.Close(); err != nil {
		return err
	}

	return nil
}

func (*mounter) NewResizeFs(_ exec.Interface) *mount.ResizeFs {
	return mount.NewResizeFs(New())
}
