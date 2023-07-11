// Copyright (c) 2021-2022 Nordix Foundation.
//
// Copyright (c) 2022-2023 Cisco and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux
// +build linux

package inject

import (
	"context"
	"strings"
	"sync"

	"github.com/NikitaSkrynnik/api/pkg/api/networkservice"
	"github.com/NikitaSkrynnik/api/pkg/api/networkservice/mechanisms/kernel"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	kernellink "github.com/NikitaSkrynnik/sdk-kernel/pkg/kernel"
	"github.com/NikitaSkrynnik/sdk-kernel/pkg/kernel/networkservice/vfconfig"
	"github.com/NikitaSkrynnik/sdk-kernel/pkg/kernel/tools/nshandle"
)

func moveInterfaceToAnotherNamespace(ifName string, fromNetNS, toNetNS netns.NsHandle) error {
	handle, err := netlink.NewHandleAt(fromNetNS)
	if err != nil {
		return errors.Wrap(err, "failed to create netlink fromNetNS handle")
	}

	link, err := handle.LinkByName(ifName)
	if err != nil {
		return errors.Wrapf(err, "failed to get net interface: %v", ifName)
	}

	if err := handle.LinkSetNsFd(link, int(toNetNS)); err != nil {
		return errors.Wrapf(err, "failed to move net interface to net NS: %v %v", ifName, toNetNS)
	}
	return nil
}

func renameInterface(origIfName, desiredIfName string, targetNetNS netns.NsHandle) error {
	handle, err := netlink.NewHandleAt(targetNetNS)
	if err != nil {
		return errors.Wrap(err, "failed to create netlink targetNetNS handle")
	}

	link, err := handle.LinkByName(origIfName)
	if err != nil {
		return errors.Wrapf(err, "failed to get net interface: %v", origIfName)
	}

	if err = handle.LinkSetDown(link); err != nil {
		return errors.Wrapf(err, "failed to down net interface: %v -> %v", origIfName, desiredIfName)
	}

	if err = handle.LinkSetName(link, desiredIfName); err != nil {
		return errors.Wrapf(err, "failed to rename net interface: %v -> %v", origIfName, desiredIfName)
	}
	return nil
}

func upInterface(ifName string, targetNetNS netns.NsHandle) error {
	handle, err := netlink.NewHandleAt(targetNetNS)
	if err != nil {
		return errors.Wrap(err, "failed to create netlink NS handle")
	}

	link, err := handle.LinkByName(ifName)
	if err != nil {
		return errors.Wrapf(err, "failed to get net interface: %v", ifName)
	}

	if err = handle.LinkSetUp(link); err != nil {
		return errors.Wrapf(err, "failed to up net interface: %v", ifName)
	}
	return nil
}

func move(ctx context.Context, conn *networkservice.Connection, vfRefCountMap map[string]int, vfRefCountMutex sync.Locker, isClient, isMoveBack bool) error {
	mech := kernel.ToMechanism(conn.GetMechanism())
	if mech == nil {
		return nil
	}

	vfConfig, ok := vfconfig.Load(ctx, isClient)
	if !ok {
		return nil
	}

	hostNetNS, err := nshandle.Current()
	if err != nil {
		return err
	}
	defer func() { _ = hostNetNS.Close() }()

	var contNetNS netns.NsHandle
	contNetNS, err = nshandle.FromURL(mech.GetNetNSURL())
	if err != nil {
		return err
	}
	if !contNetNS.IsOpen() && isMoveBack {
		contNetNS = vfConfig.ContNetNS
	}

	// keep NSE container's net ns open until connection close is done,.
	// this would properly move back VF into host net namespace even when
	// container is accidentally deleted before close.
	if !isClient || isMoveBack {
		defer func() { _ = contNetNS.Close() }()
	}

	vfRefCountMutex.Lock()
	defer vfRefCountMutex.Unlock()

	vfRefKey := vfConfig.VFPCIAddress
	if vfRefKey == "" {
		vfRefKey = vfConfig.VFInterfaceName
	}

	ifName := mech.GetInterfaceName()
	if !isMoveBack {
		err = moveToContNetNS(vfConfig, vfRefCountMap, vfRefKey, ifName, hostNetNS, contNetNS)
		if err != nil {
			// If we got an error, try to move back the vf to the host namespace
			_ = moveToHostNetNS(vfConfig, vfRefCountMap, vfRefKey, ifName, hostNetNS, contNetNS)
		} else {
			vfConfig.ContNetNS = contNetNS
		}
	} else {
		err = moveToHostNetNS(vfConfig, vfRefCountMap, vfRefKey, ifName, hostNetNS, contNetNS)
	}
	if err != nil {
		// link may not be available at this stage for cases like veth pair (might be deleted in previous chain element itself)
		// or container would have killed already (example: due to OOM error or kubectl delete)
		if strings.Contains(err.Error(), "Link not found") || strings.Contains(err.Error(), "bad file descriptor") {
			return nil
		}
		return err
	}
	return nil
}

func moveToContNetNS(vfConfig *vfconfig.VFConfig, vfRefCountMap map[string]int, vfRefKey, ifName string, hostNetNS, contNetNS netns.NsHandle) (err error) {
	if _, exists := vfRefCountMap[vfRefKey]; !exists {
		vfRefCountMap[vfRefKey] = 1
	} else {
		vfRefCountMap[vfRefKey]++
		return
	}
	link, _ := kernellink.FindHostDevice("", ifName, contNetNS)
	if link != nil {
		return
	}
	if vfConfig != nil && vfConfig.VFInterfaceName != ifName {
		err = moveInterfaceToAnotherNamespace(vfConfig.VFInterfaceName, hostNetNS, contNetNS)
		if err == nil {
			err = renameInterface(vfConfig.VFInterfaceName, ifName, contNetNS)
			if err == nil {
				err = upInterface(ifName, contNetNS)
			}
		}
	} else {
		err = moveInterfaceToAnotherNamespace(ifName, hostNetNS, contNetNS)
	}
	return err
}

func moveToHostNetNS(vfConfig *vfconfig.VFConfig, vfRefCountMap map[string]int, vfRefKey, ifName string, hostNetNS, contNetNS netns.NsHandle) error {
	var refCount int
	if count, exists := vfRefCountMap[vfRefKey]; exists && count > 0 {
		refCount = count - 1
		vfRefCountMap[vfRefKey] = refCount
	} else {
		return nil
	}

	if refCount == 0 {
		delete(vfRefCountMap, vfRefKey)
		if vfConfig != nil && vfConfig.VFInterfaceName != ifName {
			link, _ := kernellink.FindHostDevice(vfConfig.VFPCIAddress, vfConfig.VFInterfaceName, hostNetNS)
			if link != nil {
				linkName := link.GetName()
				if linkName != vfConfig.VFInterfaceName {
					if err := netlink.LinkSetName(link.GetLink(), vfConfig.VFInterfaceName); err != nil {
						return errors.Wrapf(err, "failed to rename interface from %s to %s: %v", linkName, vfConfig.VFInterfaceName, err)
					}
				}
				return nil
			}
			err := renameInterface(ifName, vfConfig.VFInterfaceName, contNetNS)
			if err == nil {
				err = moveInterfaceToAnotherNamespace(vfConfig.VFInterfaceName, contNetNS, hostNetNS)
			}
			return err
		}
		link, _ := kernellink.FindHostDevice("", ifName, hostNetNS)
		if link != nil {
			return nil
		}
		return moveInterfaceToAnotherNamespace(ifName, contNetNS, hostNetNS)
	}
	return nil
}
