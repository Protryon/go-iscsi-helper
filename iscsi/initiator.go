package iscsi

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/longhorn/go-iscsi-helper/util"
)

var (
	DeviceWaitRetryCounts   = 10
	DeviceWaitRetryInterval = 1 * time.Second

	ScsiNodesDirs = []string{
		"/etc/iscsi/nodes/",
		"/var/lib/iscsi/nodes/",
	}
)

const (
	iscsiBinary    = "iscsiadm"
	scanModeManual = "manual"
	scanModeAuto   = "auto"
	ScanTimeout    = 10 * time.Second

	shellBinary = "sh"
)

func CheckForInitiatorExistence(ne *util.NamespaceExecutor) error {
	opts := []string{
		"--version",
	}
	_, err := ne.Execute(iscsiBinary, opts)
	return err
}

func UpdateScsiDeviceTimeout(devName string, timeout int64, ne *util.NamespaceExecutor) error {
	opts := []string{
		"-c",
		fmt.Sprintf("echo %v > /sys/block/%v/device/timeout", timeout, devName),
	}
	_, err := ne.Execute(shellBinary, opts)
	return err
}

func UpdateIscsiDeviceAbortTimeout(target string, timeout int64, ne *util.NamespaceExecutor) error {
	opts := []string{
		"-m", "node",
		"-T", target,
		"-o", "update",
		"-n", "node.session.err_timeo.abort_timeout",
		"-v", strconv.FormatInt(timeout, 10),
	}
	_, err := ne.Execute(iscsiBinary, opts)
	return err
}

func DiscoverTarget(ip, target string, ne *util.NamespaceExecutor) error {
	opts := []string{
		"-m", "discovery",
		"-t", "sendtargets",
		"-p", ip,
	}
	output, err := ne.Execute(iscsiBinary, opts)
	if err != nil {
		return err
	}
	// Sometime iscsiadm won't return error but showing e.g.:
	//  iscsiadm: Could not stat /etc/iscsi/nodes//,3260,-1/default to
	//  delete node: No such file or directory\n\niscsiadm: Could not
	//  add/update [tcp:[hw=,ip=,net_if=,iscsi_if=default] 172.18.0.5,3260,1
	//  iqn.2019-10.io.longhorn:vol9]\n172.18.0.5:3260,1
	//  iqn.2019-10.io.longhorn:vol9\n"
	if strings.Contains(output, "Could not") {
		return fmt.Errorf("cannot discover target: %s", output)
	}
	if !strings.Contains(output, target) {
		return fmt.Errorf("cannot find target %s in discovered targets %s", target, output)
	}
	return nil
}

func DeleteDiscoveredTarget(ip, target string, ne *util.NamespaceExecutor) error {
	opts := []string{
		"-m", "node",
		"-o", "delete",
		"-T", target,
	}
	if ip != "" {
		opts = append(opts, "-p", ip)
	}
	_, err := ne.Execute(iscsiBinary, opts)
	return err
}

func IsTargetDiscovered(ip, target string, ne *util.NamespaceExecutor) bool {
	opts := []string{
		"-m", "node",
		"-T", target,
	}
	if ip != "" {
		opts = append(opts, "-p", ip)
	}
	_, err := ne.Execute(iscsiBinary, opts)
	return err == nil
}

func LoginTarget(ip, target string, ne *util.NamespaceExecutor) error {
	opts := []string{
		"-m", "node",
		"-T", target,
		"-p", ip,
		"--login",
	}
	_, err := ne.Execute(iscsiBinary, opts)
	if err != nil {
		return err
	}

	scanMode, err := getIscsiNodeSessionScanMode(ip, target, ne)
	if err != nil {
		return errors.Wrap(err, "Failed to get node.session.scan mode")
	}

	if scanMode == scanModeManual {
		logrus.Infof("Manually rescan LUNs of the target %v:%v", target, ip)
		if err := manualScanSession(ip, target, ne); err != nil {
			return errors.Wrapf(err, "failed to manually rescan iscsi session of target %v:%v", target, ip)
		}
	} else {
		logrus.Infof("default: automatically rescan all LUNs of all iscsi sessions")
	}

	return nil
}

// LogoutTarget will logout all sessions if ip == ""
func LogoutTarget(ip, target string, ne *util.NamespaceExecutor) error {
	opts := []string{
		"-m", "node",
		"-T", target,
		"--logout",
	}
	if ip != "" {
		opts = append(opts, "-p", ip)
	}
	_, err := ne.Execute(iscsiBinary, opts)
	return err
}

func GetDevice(ip, target string, lun int, ne *util.NamespaceExecutor) (*util.KernelDevice, error) {
	var err error

	var dev *util.KernelDevice
	for i := 0; i < DeviceWaitRetryCounts; i++ {
		dev, err = findScsiDevice(ip, target, lun, ne)
		if err == nil {
			break
		}
		time.Sleep(DeviceWaitRetryInterval)
	}
	if err != nil {
		return nil, err
	}
	return dev, nil
}

// IsTargetLoggedIn check all portals if ip == ""
func IsTargetLoggedIn(ip, target string, ne *util.NamespaceExecutor) bool {
	opts := []string{
		"-m", "session",
	}
	output, err := ne.Execute(iscsiBinary, opts)
	if err != nil {
		return false
	}
	/* It will looks like:
		tcp: [463] 172.17.0.2:3260,1 iqn.2019-10.io.longhorn:test-volume
	or:
		tcp: [463] 172.17.0.2:3260,1 iqn.2019-10.io.longhorn:test-volume (non-flash)
	*/
	found := false
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, ip+":") {
			if strings.HasSuffix(line, " "+target) ||
				strings.Contains(scanner.Text(), " "+target+" ") {
				found = true
				break
			}
		}
	}

	return found
}

func manualScanSession(ip, target string, ne *util.NamespaceExecutor) error {
	opts := []string{
		"-m", "node",
		"-T", target,
		"-p", ip,
		"--rescan",
	}
	_, err := ne.ExecuteWithTimeout(ScanTimeout, iscsiBinary, opts)
	return err
}

func getIscsiNodeSessionScanMode(ip, target string, ne *util.NamespaceExecutor) (string, error) {
	opts := []string{
		"-m", "node",
		"-T", target,
		"-p", ip,
		"-o", "show",
	}
	output, err := ne.ExecuteWithTimeout(ScanTimeout, iscsiBinary, opts)
	if err != nil {
		return "", err
	}
	if strings.Contains(output, "node.session.scan = manual") {
		return scanModeManual, nil
	}
	return scanModeAuto, nil
}

func findScsiDevice(ip, target string, lun int, ne *util.NamespaceExecutor) (*util.KernelDevice, error) {
	name := ""

	opts := []string{
		"-m", "session",
		"-P", "3",
	}
	output, err := ne.Execute(iscsiBinary, opts)
	if err != nil {
		return nil, err
	}
	/*
		Now we got something like this in output, and need to parse it
		Target: iqn.2019-10.io.longhorn:for.all (non-flash)
			Current Portal: 172.17.0.2:3260,1
			Persistent Portal: 172.17.0.2:3260,1
			...
			Attached SCSI devices:
			...
			scsi12 Channel 00 Id 0 Lun: 0
			scsi12 Channel 00 Id 0 Lun: 1
				Attached scsi disk sdb		State: running
		...
		Target: ...
	*/
	scanner := bufio.NewScanner(strings.NewReader(output))
	targetLine := "Target: " + target
	ipLine := " " + ip + ":"
	lunLine := "Lun: " + strconv.Itoa(lun)
	diskPrefix := "Attached scsi disk"
	stateLine := "State:"

	inTarget := false
	inIP := false
	inLun := false
	for scanner.Scan() {
		/* Target line can be:
			Target: iqn.2019-10.io.longhorn:for.all (non-flash)
		or:
			Target: iqn.2019-10.io.longhorn:for.all
		*/
		if !inTarget &&
			(strings.Contains(scanner.Text(), targetLine+" ") ||
				strings.HasSuffix(scanner.Text(), targetLine)) {
			inTarget = true
			continue
		}
		if inTarget && strings.Contains(scanner.Text(), ipLine) {
			inIP = true
			continue
		}
		if inIP && strings.Contains(scanner.Text(), lunLine) {
			inLun = true
			continue
		}
		// The line we need
		if inLun {
			line := scanner.Text()
			if !strings.Contains(line, diskPrefix) {
				return nil, fmt.Errorf("invalid output format, cannot find disk in: %s\n %s", line, output)
			}
			line = strings.TrimSpace(strings.Split(line, stateLine)[0])
			line = strings.TrimPrefix(line, diskPrefix)
			name = strings.TrimSpace(line)
			break
		}
	}

	if name == "" {
		return nil, fmt.Errorf("cannot find iSCSI device")
	}

	// now that we know the device is mapped, we can get it's (major:minor)
	devices, err := util.GetKnownDevices(ne)
	if err != nil {
		return nil, err
	}

	dev, known := devices[name]
	if !known {
		return nil, fmt.Errorf("cannot find kernel device for iSCSI device: %s", name)
	}

	return dev, nil
}

func listFiles(path string) ([]string, error) {
	var out []string
	var walk = func(path string, entry fs.DirEntry, err error) error {
		out = append(out, path)
		return nil
	}
	err := filepath.WalkDir(path, walk)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func CleanupScsiNodes(target string, ne *util.NamespaceExecutor) error {
	for _, dir := range ScsiNodesDirs {
		_, err := util.ForkAndSwitchToNamespace(ne.GetNamespace(), time.Minute, func() (*interface{}, error) {
			_, err := util.NsStat(dir)
			return nil, err
		})
		if err != nil {
			continue
		}
		targetDir := filepath.Join(dir, target)
		_, err = util.ForkAndSwitchToNamespace(ne.GetNamespace(), time.Minute, func() (*interface{}, error) {
			_, err := util.NsStat(targetDir)
			return nil, err
		})
		if err != nil {
			continue
		}
		// Remove all empty files in the directory
		output, err := util.ForkAndSwitchToNamespace(ne.GetNamespace(), time.Minute, func() (*[]string, error) {
			output, err := listFiles(targetDir)
			return &output, err
		})
		if err != nil {
			return errors.Wrapf(err, "failed to search iSCSI directory %v", targetDir)
		}
		for _, file := range *output {
			output, err := util.ForkAndSwitchToNamespace(ne.GetNamespace(), time.Minute, func() (*fs.FileInfo, error) {
				output, err := util.NsStat(file)
				return &output, err
			})
			if err != nil {
				return errors.Wrapf(err, "failed to check iSCSI node file %v", file)
			}
			if (*output).Size() == 0 && (*output).Mode().IsRegular() {
				_, err = util.ForkAndSwitchToNamespace(ne.GetNamespace(), time.Minute, func() (*interface{}, error) {
					return nil, os.Remove(file)
				})
				if err != nil {
					return errors.Wrapf(err, "failed to clean up empty iSCSI node file %v", file)
				}
				// We're trying to clean up the upper level directory as well, but won't mind if we fail
				_, _ = util.ForkAndSwitchToNamespace(ne.GetNamespace(), time.Minute, func() (*interface{}, error) {
					return nil, os.Remove(filepath.Dir(file))
				})
			}
		}
	}
	return nil
}

func RescanTarget(ip, target string, ne *util.NamespaceExecutor) error {
	opts := []string{
		"-m", "node",
		"-T", target,
		"-R",
	}
	if ip != "" {
		opts = append(opts, "-p", ip)
	}
	_, err := ne.Execute(iscsiBinary, opts)
	return err
}
