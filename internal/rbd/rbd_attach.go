/*
Copyright 2018 The Ceph-CSI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rbd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ceph/ceph-csi/internal/util"

	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	rbdTonbd  = "rbd-nbd"
	moduleNbd = "nbd"

	accessTypeKRbd = "krbd"
	accessTypeNbd  = "nbd"

	rbd = "rbd"

	// Output strings returned during invocation of "rbd unmap --device-type... <imageSpec>" when
	// image is not found to be mapped. Used to ignore errors when attempting to unmap such images.
	// The %s format specifier should contain the <imageSpec> string
	// NOTE: When using devicePath instead of imageSpec, the error strings are different.
	rbdUnmapCmdkRbdMissingMap = "rbd: %s: not a mapped image or snapshot"
	rbdUnmapCmdNbdMissingMap  = "rbd-nbd: %s is not mapped"
	rbdMapConnectionTimeout   = "Connection timed out"

	defaultNbdReAttachTimeout = 300 /* in seconds */
	defaultNbdIOTimeout       = 0   /* do not abort the requests */

	// The default way of creating nbd devices via rbd-nbd is through the
	// legacy ioctl interface, to take advantage of netlink features we
	// should specify `try-netlink` flag explicitly.
	useNbdNetlink = "try-netlink"

	// `reattach-timeout` of rbd-nbd is to tweak NBD_ATTR_DEAD_CONN_TIMEOUT.
	// It specifies how long the device should be held waiting for the
	// userspace process to come back to life.
	setNbdReattach = "reattach-timeout"

	// `io-timeout` of rbd-nbd is to tweak NBD_ATTR_TIMEOUT. It specifies
	// how long the IO should wait to get handled before bailing out.
	setNbdIOTimeout = "io-timeout"
)

var hasNBD = false

func init() {
	hasNBD = checkRbdNbdTools()
}

// rbdDeviceInfo strongly typed JSON spec for rbd device list output (of type krbd).
type rbdDeviceInfo struct {
	ID             string `json:"id"`
	Pool           string `json:"pool"`
	RadosNamespace string `json:"namespace"`
	Name           string `json:"name"`
	Device         string `json:"device"`
}

// nbdDeviceInfo strongly typed JSON spec for rbd-nbd device list output (of type nbd)
// NOTE: There is a bug in rbd output that returns id as number for nbd, and string for krbd, thus
// requiring 2 different JSON structures to unmarshal the output.
// NOTE: image key is "name" in krbd output and "image" in nbd output, which is another difference.
type nbdDeviceInfo struct {
	ID             int64  `json:"id"`
	Pool           string `json:"pool"`
	RadosNamespace string `json:"namespace"`
	Name           string `json:"image"`
	Device         string `json:"device"`
}

type detachRBDImageArgs struct {
	imageOrDeviceSpec string
	isImageSpec       bool
	isNbd             bool
	encrypted         bool
	volumeID          string
	unmapOptions      string
	logDir            string
}

// rbdGetDeviceList queries rbd about mapped devices and returns a list of rbdDeviceInfo
// It will selectively list devices mapped using krbd or nbd as specified by accessType.
func rbdGetDeviceList(ctx context.Context, accessType string) ([]rbdDeviceInfo, error) {
	// rbd device list --format json --device-type [krbd|nbd]
	var (
		rbdDeviceList []rbdDeviceInfo
		nbdDeviceList []nbdDeviceInfo
	)

	stdout, _, err := util.ExecCommand(ctx, rbd, "device", "list", "--format="+"json", "--device-type", accessType)
	if err != nil {
		return nil, fmt.Errorf("error getting device list from rbd for devices of type (%s): %w", accessType, err)
	}

	if accessType == accessTypeKRbd {
		err = json.Unmarshal([]byte(stdout), &rbdDeviceList)
	} else {
		err = json.Unmarshal([]byte(stdout), &nbdDeviceList)
	}
	if err != nil {
		return nil, fmt.Errorf(
			"error to parse JSON output of device list for devices of type (%s): %w",
			accessType,
			err)
	}

	// convert output to a rbdDeviceInfo list for consumers
	if accessType == accessTypeNbd {
		for _, device := range nbdDeviceList {
			rbdDeviceList = append(
				rbdDeviceList,
				rbdDeviceInfo{
					ID:             strconv.FormatInt(device.ID, 10),
					Pool:           device.Pool,
					RadosNamespace: device.RadosNamespace,
					Name:           device.Name,
					Device:         device.Device,
				})
		}
	}

	return rbdDeviceList, nil
}

// findDeviceMappingImage finds a devicePath, if available, based on image spec (pool/{namespace/}image) on the node.
func findDeviceMappingImage(ctx context.Context, pool, namespace, image string, useNbdDriver bool) (string, bool) {
	accessType := accessTypeKRbd
	if useNbdDriver {
		accessType = accessTypeNbd
	}

	imageSpec := fmt.Sprintf("%s/%s", pool, image)
	if namespace != "" {
		imageSpec = fmt.Sprintf("%s/%s/%s", pool, namespace, image)
	}

	rbdDeviceList, err := rbdGetDeviceList(ctx, accessType)
	if err != nil {
		util.WarningLog(ctx, "failed to determine if image (%s) is mapped to a device (%v)", imageSpec, err)

		return "", false
	}

	for _, device := range rbdDeviceList {
		if device.Name == image && device.Pool == pool && device.RadosNamespace == namespace {
			return device.Device, true
		}
	}

	return "", false
}

// Stat a path, if it doesn't exist, retry maxRetries times.
func waitForPath(ctx context.Context, pool, namespace, image string, maxRetries int, useNbdDriver bool) (string, bool) {
	for i := 0; i < maxRetries; i++ {
		if i != 0 {
			time.Sleep(time.Second)
		}

		device, found := findDeviceMappingImage(ctx, pool, namespace, image, useNbdDriver)
		if found {
			return device, found
		}
	}

	return "", false
}

// Check if rbd-nbd tools are installed.
func checkRbdNbdTools() bool {
	// check if the module is loaded or compiled in
	_, err := os.Stat(fmt.Sprintf("/sys/module/%s", moduleNbd))
	if os.IsNotExist(err) {
		// try to load the module
		_, _, err = util.ExecCommand(context.TODO(), "modprobe", moduleNbd)
		if err != nil {
			util.ExtendedLogMsg("rbd-nbd: nbd modprobe failed with error %v", err)

			return false
		}
	}
	if _, _, err := util.ExecCommand(context.TODO(), rbdTonbd, "--version"); err != nil {
		util.ExtendedLogMsg("rbd-nbd: running rbd-nbd --version failed with error %v", err)

		return false
	}
	util.ExtendedLogMsg("rbd-nbd tools were found.")

	return true
}

func attachRBDImage(ctx context.Context, volOptions *rbdVolume, device string, cr *util.Credentials) (string, error) {
	var err error

	image := volOptions.RbdImageName
	useNBD := false
	if volOptions.Mounter == rbdTonbd && hasNBD {
		useNBD = true
	}

	devicePath, found := waitForPath(ctx, volOptions.Pool, volOptions.RadosNamespace, image, 1, useNBD)
	if !found {
		backoff := wait.Backoff{
			Duration: rbdImageWatcherInitDelay,
			Factor:   rbdImageWatcherFactor,
			Steps:    rbdImageWatcherSteps,
		}

		err = waitForrbdImage(ctx, backoff, volOptions)

		if err != nil {
			return "", err
		}
		devicePath, err = createPath(ctx, volOptions, device, cr)
	}

	return devicePath, err
}

func appendDeviceTypeAndOptions(cmdArgs []string, isNbd, isThick bool, userOptions string) []string {
	accessType := accessTypeKRbd
	if isNbd {
		accessType = accessTypeNbd
	}

	cmdArgs = append(cmdArgs, "--device-type", accessType)
	if !isNbd {
		// Enable mapping and unmapping images from a non-initial network
		// namespace (e.g. for Multus CNI).  The network namespace must be
		// owned by the initial user namespace.
		cmdArgs = append(cmdArgs, "--options", "noudev")
	} else {
		if !strings.Contains(userOptions, useNbdNetlink) {
			cmdArgs = append(cmdArgs, "--options", useNbdNetlink)
		}
		if !strings.Contains(userOptions, setNbdReattach) {
			cmdArgs = append(cmdArgs, "--options", fmt.Sprintf("%s=%d", setNbdReattach, defaultNbdReAttachTimeout))
		}
		if !strings.Contains(userOptions, setNbdIOTimeout) {
			cmdArgs = append(cmdArgs, "--options", fmt.Sprintf("%s=%d", setNbdIOTimeout, defaultNbdIOTimeout))
		}
	}
	if isThick {
		// When an image is thick-provisioned, any discard/unmap/trim
		// requests should not free extents.
		cmdArgs = append(cmdArgs, "--options", "notrim")
	}
	if userOptions != "" {
		// userOptions is appended after, possibly overriding the above
		// default options.
		cmdArgs = append(cmdArgs, "--options", userOptions)
	}

	return cmdArgs
}

// appendRbdNbdCliOptions append mandatory options and convert list of useroptions
// provided for rbd integrated cli to rbd-nbd cli format specific.
func appendRbdNbdCliOptions(cmdArgs []string, userOptions string) []string {
	if !strings.Contains(userOptions, useNbdNetlink) {
		cmdArgs = append(cmdArgs, fmt.Sprintf("--%s", useNbdNetlink))
	}
	if !strings.Contains(userOptions, setNbdReattach) {
		cmdArgs = append(cmdArgs, fmt.Sprintf("--%s=%d", setNbdReattach, defaultNbdReAttachTimeout))
	}
	if !strings.Contains(userOptions, setNbdIOTimeout) {
		cmdArgs = append(cmdArgs, fmt.Sprintf("--%s=%d", setNbdIOTimeout, defaultNbdIOTimeout))
	}
	if userOptions != "" {
		options := strings.Split(userOptions, ",")
		for _, opt := range options {
			cmdArgs = append(cmdArgs, fmt.Sprintf("--%s", opt))
		}
	}

	return cmdArgs
}

func createPath(ctx context.Context, volOpt *rbdVolume, device string, cr *util.Credentials) (string, error) {
	isNbd := false
	imagePath := volOpt.String()

	util.TraceLog(ctx, "rbd: map mon %s", volOpt.Monitors)

	mapArgs := []string{
		"--id", cr.ID,
		"-m", volOpt.Monitors,
		"--keyfile=" + cr.KeyFile,
	}

	// Choose access protocol
	if volOpt.Mounter == rbdTonbd && hasNBD {
		isNbd = true
	}

	// check if the image should stay thick-provisioned
	isThick, err := volOpt.isThickProvisioned()
	if err != nil {
		util.WarningLog(ctx, "failed to detect if image %q is thick-provisioned: %v", volOpt, err)
	}

	if isNbd {
		mapArgs = append(mapArgs, "--log-file",
			getCephClientLogFileName(volOpt.VolID, volOpt.LogDir, "rbd-nbd"))
	}

	cli := rbd
	if device != "" {
		// TODO: use rbd cli for attach/detach in the future
		cli = rbdNbdMounter
		mapArgs = append(mapArgs, "attach", imagePath, "--device", device)
		mapArgs = appendRbdNbdCliOptions(mapArgs, volOpt.MapOptions)
	} else {
		mapArgs = append(mapArgs, "map", imagePath)
		mapArgs = appendDeviceTypeAndOptions(mapArgs, isNbd, isThick, volOpt.MapOptions)
	}

	if volOpt.readOnly {
		mapArgs = append(mapArgs, "--read-only")
	}

	// Execute map
	stdout, stderr, err := util.ExecCommand(ctx, cli, mapArgs...)
	if err != nil {
		util.WarningLog(ctx, "rbd: map error %v, rbd output: %s", err, stderr)
		// unmap rbd image if connection timeout
		if strings.Contains(err.Error(), rbdMapConnectionTimeout) {
			dArgs := detachRBDImageArgs{
				imageOrDeviceSpec: imagePath,
				isImageSpec:       true,
				isNbd:             isNbd,
				encrypted:         volOpt.isEncrypted(),
				volumeID:          volOpt.VolID,
				unmapOptions:      volOpt.UnmapOptions,
				logDir:            volOpt.LogDir,
			}
			detErr := detachRBDImageOrDeviceSpec(ctx, dArgs)
			if detErr != nil {
				util.WarningLog(ctx, "rbd: %s unmap error %v", imagePath, detErr)
			}
		}

		return "", fmt.Errorf("rbd: map failed with error %w, rbd error output: %s", err, stderr)
	}
	devicePath := strings.TrimSuffix(stdout, "\n")

	return devicePath, nil
}

func waitForrbdImage(ctx context.Context, backoff wait.Backoff, volOptions *rbdVolume) error {
	imagePath := volOptions.String()

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		used, err := volOptions.isInUse()
		if err != nil {
			return false, fmt.Errorf("fail to check rbd image status: (%w)", err)
		}
		if (volOptions.DisableInUseChecks) && (used) {
			util.UsefulLog(ctx, "valid multi-node attach requested, ignoring watcher in-use result")

			return used, nil
		}

		return !used, nil
	})
	// return error if rbd image has not become available for the specified timeout
	if errors.Is(err, wait.ErrWaitTimeout) {
		return fmt.Errorf("rbd image %s is still being used", imagePath)
	}
	// return error if any other errors were encountered during waiting for the image to become available
	return err
}

func detachRBDDevice(ctx context.Context, devicePath, volumeID, unmapOptions string, encrypted bool) error {
	nbdType := false
	if strings.HasPrefix(devicePath, "/dev/nbd") {
		nbdType = true
	}

	dArgs := detachRBDImageArgs{
		imageOrDeviceSpec: devicePath,
		isImageSpec:       false,
		isNbd:             nbdType,
		encrypted:         encrypted,
		volumeID:          volumeID,
		unmapOptions:      unmapOptions,
	}

	return detachRBDImageOrDeviceSpec(ctx, dArgs)
}

// detachRBDImageOrDeviceSpec detaches an rbd imageSpec or devicePath, with additional checking
// when imageSpec is used to decide if image is already unmapped.
func detachRBDImageOrDeviceSpec(
	ctx context.Context,
	dArgs detachRBDImageArgs) error {
	if dArgs.encrypted {
		mapperFile, mapperPath := util.VolumeMapper(dArgs.volumeID)
		mappedDevice, mapper, err := util.DeviceEncryptionStatus(ctx, mapperPath)
		if err != nil {
			util.ErrorLog(ctx, "error determining LUKS device on %s, %s: %s",
				mapperPath, dArgs.imageOrDeviceSpec, err)

			return err
		}
		if len(mapper) > 0 {
			// mapper found, so it is open Luks device
			err = util.CloseEncryptedVolume(ctx, mapperFile)
			if err != nil {
				util.ErrorLog(ctx, "error closing LUKS device on %s, %s: %s",
					mapperPath, dArgs.imageOrDeviceSpec, err)

				return err
			}
			dArgs.imageOrDeviceSpec = mappedDevice
		}
	}

	unmapArgs := []string{"unmap", dArgs.imageOrDeviceSpec}
	unmapArgs = appendDeviceTypeAndOptions(unmapArgs, dArgs.isNbd, false, dArgs.unmapOptions)

	_, stderr, err := util.ExecCommand(ctx, rbd, unmapArgs...)
	if err != nil {
		// Messages for krbd and nbd differ, hence checking either of them for missing mapping
		// This is not applicable when a device path is passed in
		if dArgs.isImageSpec &&
			(strings.Contains(stderr, fmt.Sprintf(rbdUnmapCmdkRbdMissingMap, dArgs.imageOrDeviceSpec)) ||
				strings.Contains(stderr, fmt.Sprintf(rbdUnmapCmdNbdMissingMap, dArgs.imageOrDeviceSpec))) {
			// Devices found not to be mapped are treated as a successful detach
			util.TraceLog(ctx, "image or device spec (%s) not mapped", dArgs.imageOrDeviceSpec)

			return nil
		}

		return fmt.Errorf("rbd: unmap for spec (%s) failed (%w): (%s)", dArgs.imageOrDeviceSpec, err, stderr)
	}
	if dArgs.isNbd && dArgs.logDir != "" {
		logFile := getCephClientLogFileName(dArgs.volumeID, dArgs.logDir, "rbd-nbd")
		if err = os.Remove(logFile); err != nil {
			util.WarningLog(ctx, "failed to remove logfile: %s, error: %v",
				logFile, err)
		}
	}

	return nil
}
