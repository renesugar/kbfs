// +build darwin

package libfuse

import (
	"errors"

	"bazil.org/fuse"
)

func getPlatformSpecificMountOptions(dir string, platformParams PlatformParams) ([]fuse.MountOption, error) {
	options := []fuse.MountOption{}

	var locationOption fuse.MountOption
	if platformParams.UseSystemFuse {
		// Only allow osxfuse 3.x.
		locationOption = fuse.OSXFUSELocations(fuse.OSXFUSELocationV3)
	} else {
		// Only allow kbfuse.
		kbfusePath := fuse.OSXFUSEPaths{
			DevicePrefix: "/dev/kbfuse",
			Load:         "/Library/Filesystems/kbfuse.fs/Contents/Resources/load_kbfuse",
			Mount:        "/Library/Filesystems/kbfuse.fs/Contents/Resources/mount_kbfuse",
			DaemonVar:    "MOUNT_KBFUSE_DAEMON_PATH",
		}
		locationOption = fuse.OSXFUSELocations(kbfusePath)
	}
	options = append(options, locationOption)

	// Volume name option is only used on OSX (ignored on other platforms).
	volName, err := volumeName(dir)
	if err != nil {
		return nil, err
	}

	options = append(options, fuse.VolumeName(volName))

	return options, nil
}

func translatePlatformSpecificError(err error, platformParams PlatformParams) error {
	// TODO: Have a better way to detect this case.
	if err.Error() == "cannot locate OSXFUSE" {
		if platformParams.UseSystemFuse {
			return errors.New(
				"cannot locate OSXFUSE 3.x (3.2 recommended)")
		}
		return errors.New(
			"cannot locate kbfuse; either install the Keybase " +
				"app, or install OSXFUSE 3.x (3.2 " +
				"recommended) and pass in --use-system-fuse")
	}
	return err
}
