/*
Copyright 2020 The Rook Authors. All rights reserved.

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

package osd

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/pkg/errors"
	v1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/daemon/ceph/osd/kms"
	oposd "github.com/rook/rook/pkg/operator/ceph/cluster/osd"
	"github.com/rook/rook/pkg/util"
)

const (
	cryptsetupBinary                = "cryptsetup"
	dmsetupBinary                   = "dmsetup"
	luksOpenCmdTimeOut              = 90 * time.Second
	removeEncryptedDeviceCmdTimeOut = 30 * time.Second
)

var luksLabelCephFSID = regexp.MustCompile("ceph_fsid=(.*)")

func CloseEncryptedDevice(context *clusterd.Context, dmName string) error {
	args := []string{"--verbose", "luksClose", dmName}
	cryptsetupOut, err := context.Executor.ExecuteCommandWithCombinedOutput(cryptsetupBinary, args...)
	if err != nil {
		return errors.Wrapf(err, "failed to close encrypted device. %s", cryptsetupOut)
	}

	logger.Infof("dm version:\n%s", cryptsetupOut)
	return nil
}

func dmsetupVersion(context *clusterd.Context) error {
	args := []string{"version"}
	dmsetupOut, err := context.Executor.ExecuteCommandWithCombinedOutput(dmsetupBinary, args...)
	if err != nil {
		return errors.Wrapf(err, "failed to find device mapper version. %s", dmsetupOut)
	}

	logger.Info(dmsetupOut)
	return nil
}

func setKEKinEnv(context *clusterd.Context, clusterInfo *cephclient.ClusterInfo) error {
	// KMS details are passed by the Operator as env variables in the pod
	// The token if any is mounted in the provisioner pod as an env variable so the secrets lib will
	// pick it up
	clusterSpec := &v1.ClusterSpec{Security: v1.ClusterSecuritySpec{KeyManagementService: v1.KeyManagementServiceSpec{ConnectionDetails: kms.ConfigEnvsToMapString()}}}

	// The ibm key protect library does not read any environment variables, so we must set the
	// service api key (coming from the secret mounted as environment variable) in the KMS
	// connection details. These details are used to build the client connection
	if clusterSpec.Security.KeyManagementService.IsIBMKeyProtectKMS() {
		ibmServiceApiKey := os.Getenv(kms.IbmKeyProtectServiceApiKey)
		if ibmServiceApiKey == "" {
			return errors.Errorf("ibm key protect %q environment variable is not set", kms.IbmKeyProtectServiceApiKey)
		}
		clusterSpec.Security.KeyManagementService.ConnectionDetails[kms.IbmKeyProtectServiceApiKey] = ibmServiceApiKey
	}

	if clusterSpec.Security.KeyManagementService.IsKMIPKMS() {
		// the following files will be mounted to the osd pod.
		byteValue, err := os.ReadFile(path.Join(kms.EtcKmipDir, kms.KmipCACertFileName))
		if err != nil {
			return errors.Wrapf(err, "failed to read file %q", kms.KmipCACertFileName)
		}
		clusterSpec.Security.KeyManagementService.ConnectionDetails[kms.KmipCACert] = string(byteValue)

		byteValue, err = os.ReadFile(path.Join(kms.EtcKmipDir, kms.KmipClientCertFileName))
		if err != nil {
			return errors.Wrapf(err, "failed to read file %q", kms.KmipClientCertFileName)
		}
		clusterSpec.Security.KeyManagementService.ConnectionDetails[kms.KmipClientCert] = string(byteValue)

		byteValue, err = os.ReadFile(path.Join(kms.EtcKmipDir, kms.KmipClientKeyFileName))
		if err != nil {
			return errors.Wrapf(err, "failed to read file %q", kms.KmipClientKeyFileName)
		}
		clusterSpec.Security.KeyManagementService.ConnectionDetails[kms.KmipClientKey] = string(byteValue)
	}

	kmsConfig := kms.NewConfig(context, clusterSpec, clusterInfo)

	// Fetch the KEK
	kek, err := kmsConfig.GetSecret(os.Getenv(oposd.PVCNameEnvVarName))
	if err != nil {
		return errors.Wrapf(err, "failed to retrieve key encryption key from %q kms", kmsConfig.Provider)
	}

	if kek == "" {
		return errors.New("key encryption key is empty")
	}

	// Set the KEK as an env variable for ceph-volume
	err = os.Setenv(oposd.CephVolumeEncryptedKeyEnvVarName, kek)
	if err != nil {
		return errors.Wrap(err, "failed to set key encryption key env variable for ceph-volume")
	}

	logger.Debug("successfully set kek to env variable")
	return nil
}

func setLUKSLabelAndSubsystem(context *clusterd.Context, clusterInfo *cephclient.ClusterInfo, disk string) error {
	// The PVC info is a nice to have
	pvcName := os.Getenv(oposd.PVCNameEnvVarName)
	if pvcName == "" {
		return errors.Errorf("failed to find %q environment variable", oposd.PVCNameEnvVarName)
	}
	subsystem := fmt.Sprintf("ceph_fsid=%s", clusterInfo.FSID)
	label := fmt.Sprintf("pvc_name=%s", pvcName)

	logger.Infof("setting LUKS subsystem to %q and label to %q to disk %q", subsystem, label, disk)
	// 48 characters limit for both label and subsystem
	args := []string{"config", disk, "--subsystem", subsystem, "--label", label}
	output, err := context.Executor.ExecuteCommandWithCombinedOutput(cryptsetupBinary, args...)
	if err != nil {
		return errors.Wrapf(err, "failed to set subsystem %q and label %q to encrypted device %q. is your distro built with LUKS1 as a default?. %s", subsystem, label, disk, output)
	}

	logger.Infof("successfully set LUKS subsystem to %q and label to %q to disk %q", subsystem, label, disk)
	return nil
}

func dumpLUKS(context *clusterd.Context, disk string) (string, error) {
	args := []string{"luksDump", disk}
	cryptsetupOut, err := context.Executor.ExecuteCommandWithCombinedOutput(cryptsetupBinary, args...)
	if err != nil {
		return "", errors.Wrapf(err, "failed to dump LUKS header for disk %q. %s", disk, cryptsetupOut)
	}

	return cryptsetupOut, nil
}

func openEncryptedDevice(context *clusterd.Context, disk, target, passphrase string) error {
	args := []string{"luksOpen", "--verbose", "--allow-discards", disk, target}
	err := context.Executor.ExecuteCommandWithStdin(luksOpenCmdTimeOut, cryptsetupBinary, &passphrase, args...)
	if err != nil {
		return errors.Wrapf(err, "failed to open encrypted device %q", disk)
	}

	return nil
}

// removeEncryptionKeySlot removes the given key slot from the target disk.
// This function ignores error indicating that the given key slot is not active.
func removeEncryptionKeySlot(context *clusterd.Context, disk, passphrase, slot string) error {
	passphraseFile, err := util.CreateTempFile(passphrase)
	if err != nil {
		return errors.Wrapf(err, "failed to create passphrase file")
	}
	defer os.Remove(passphraseFile.Name())

	args := []string{
		"--verbose",
		fmt.Sprintf("--key-file=%s", passphraseFile.Name()),
		"luksKillSlot",
		disk,
		slot,
	}
	output, err := context.Executor.ExecuteCommandWithTimeout(luksOpenCmdTimeOut,
		cryptsetupBinary, args...)
	// ignore the error if the key slot is not active.
	if err != nil && !strings.Contains(err.Error()+output, fmt.Sprintf("Keyslot %s is not active", slot)) {
		return errors.Wrapf(err, "failed to remove key slot %q of encrypted device %q: %q",
			slot, disk, output)
	}

	return nil
}

// ensureEncryptionKey ensures the given key is in the given slot of the target disk.
// If the error, received from lukChangeKey cmd, shows that the key did not match,
// the function will return false and no error signify that the given key is not in the slot.
func ensureEncryptionKey(context *clusterd.Context, disk, passphrase, slot string) (bool, error) {
	passphraseFile, err := util.CreateTempFile(passphrase)
	if err != nil {
		return false, errors.Wrapf(err, "failed to create passphrase file")
	}
	defer os.Remove(passphraseFile.Name())

	args := []string{
		"--verbose",
		fmt.Sprintf("--key-file=%s", passphraseFile.Name()),
		fmt.Sprintf("--key-slot=%s", slot),
		"luksChangeKey",
		disk,
		passphraseFile.Name(),
	}
	output, err := context.Executor.ExecuteCommandWithTimeout(luksOpenCmdTimeOut,
		cryptsetupBinary, args...)
	if err != nil {
		if strings.Contains(err.Error()+output, "No key available with this passphrase") {
			// ignore the error if the key does not match the one in key slot and return false
			// to signify no match.
			return false, nil
		}

		return false, errors.Wrapf(err, "failed to ensure passphrase in slot %q of encrypted device %q: %q",
			slot, disk, output)
	}

	return true, nil
}

// addEncryptionKey adds a new key to the given slot of the target disk.
// If the given slot is filled:
// - a check is done to see if the key in the give slot is the same as the new key
//   - if the key is the same, nothing is done.
//   - else, the slot is wiped and the new key is added.
func addEncryptionKey(context *clusterd.Context, disk, passphrase, newPassphrase, slot string) error {
	passphraseFile, err := util.CreateTempFile(passphrase)
	if err != nil {
		return errors.Wrapf(err, "failed to create passphrase file")
	}
	defer os.Remove(passphraseFile.Name())

	newPassphraseFile, err := util.CreateTempFile(newPassphrase)
	if err != nil {
		return errors.Wrapf(err, "failed to create new passphrase file")
	}
	defer os.Remove(newPassphraseFile.Name())

	args := []string{
		"--verbose",
		fmt.Sprintf("--key-file=%s", passphraseFile.Name()),
		fmt.Sprintf("--key-slot=%s", slot),
		"luksAddKey",
		disk,
		newPassphraseFile.Name(),
	}
	output, err := context.Executor.ExecuteCommandWithTimeout(luksOpenCmdTimeOut,
		cryptsetupBinary, args...)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error()+output, fmt.Sprintf("Key slot %s is full", slot)) {
		// run ensureEncryptionKey to make sure the newPassphrase is the one that is set.
		matched, err := ensureEncryptionKey(context, disk, newPassphrase, slot)
		if err != nil {
			return errors.Wrapf(err, "failed to ensure passphrase in slot %q of encrypted device %q", slot, disk)
		}
		// if newPassphrase is not one in the slot, then remove the key slot using current passphrase and then
		// add the newPassphrase to it.
		if !matched {
			err = removeEncryptionKeySlot(context, disk, passphrase, slot)
			if err != nil {
				return errors.Wrapf(err, "failed to remove key slot %q of encrypted device %q", slot, disk)
			}
			err = addEncryptionKey(context, disk, passphrase, newPassphrase, slot)
			if err != nil {
				return errors.Wrapf(err, "failed to add new passphrase to slot %q encrypted device %q", slot, disk)
			}
		}
		return nil
	}

	return errors.Wrapf(err, "failed to add new passphrase to encrypted device %q: %q",
		disk, output)
}

func RemoveEncryptedDevice(context *clusterd.Context, target string) error {
	args := []string{"remove", "--force", target}
	output, err := context.Executor.ExecuteCommandWithTimeout(removeEncryptedDeviceCmdTimeOut, "dmsetup", args...)
	// ignore error if no device was found.
	if err != nil {
		return errors.Wrapf(err, "failed to remove dm device %q: %q", target, output)
	}
	logger.Debugf("successfully removed stale dm device %q", target)

	return nil
}

func isCephEncryptedBlock(context *clusterd.Context, currentClusterFSID string, disk string) bool {
	metadata, err := dumpLUKS(context, disk)
	if err != nil {
		logger.Errorf("failed to determine if the encrypted block %q is from our cluster. %v", disk, err)
		return false
	}

	// Now we parse the CLI output
	// JSON output is only available with cryptsetup 2.4.x - https://gitlab.com/cryptsetup/cryptsetup/-/issues/511
	ceph_fsid := luksLabelCephFSID.FindString(metadata)
	if ceph_fsid == "" {
		logger.Error("failed to find ceph_fsid in the LUKS header, the encrypted disk is not from a ceph cluster")
		return false
	}

	// is it an OSD from our cluster?
	currentDiskCephFSID := strings.SplitAfter(ceph_fsid, "=")[1]
	if currentDiskCephFSID != currentClusterFSID {
		logger.Errorf("encrypted disk %q is part of a different ceph cluster %q", disk, currentDiskCephFSID)
		return false
	}

	return true
}
