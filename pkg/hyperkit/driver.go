// +build darwin

/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package hyperkit

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	hyperkitdriver "github.com/code-ready/machine/drivers/hyperkit"
	"github.com/code-ready/machine/libmachine/drivers"
	"github.com/code-ready/machine/libmachine/log"
	"github.com/code-ready/machine/libmachine/state"
	ps "github.com/mitchellh/go-ps"
	hyperkit "github.com/moby/hyperkit/go"
	"github.com/pkg/errors"
)

const (
	pidFileName     = "hyperkit.pid"
	machineFileName = "hyperkit.json"
	permErr         = "%s needs to run with elevated permissions. " +
		"Please run the following command, then try again: " +
		"sudo chown root:wheel %s && sudo chmod u+s %s"
)

type Driver hyperkitdriver.Driver

// NewDriver creates a new driver for a host
func NewDriver() *Driver {
	return &Driver{
		VMDriver: &drivers.VMDriver{
			BaseDriver: &drivers.BaseDriver{},
			CPU:        DefaultCPUs,
			Memory:     DefaultMemory,
		},
	}
}

// PreCreateCheck is called to enforce pre-creation steps
func (d *Driver) PreCreateCheck() error {
	return d.verifyRootPermissions()
}

// verifyRootPermissions is called before any step which needs root access
func (d *Driver) verifyRootPermissions() error {
	if !d.VMNet {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	euid := syscall.Geteuid()
	log.Debugf("exe=%s uid=%d", exe, euid)
	if euid != 0 {
		return fmt.Errorf(permErr, filepath.Base(exe), exe, exe)
	}
	return nil
}

func (d *Driver) getDiskPath() string {
	return d.ResolveStorePath(fmt.Sprintf("%s.%s", d.MachineName, d.ImageFormat))
}

// Create a host using the driver's config
func (d *Driver) Create() error {
	if err := d.verifyRootPermissions(); err != nil {
		return err
	}

	if err := copyFile(d.ImageSourcePath, d.getDiskPath()); err != nil {
		return err
	}

	return d.Start()
}

// DriverName returns the name of the driver
func (d *Driver) DriverName() string {
	return DriverName
}

// GetSSHHostname returns hostname for use with ssh
func (d *Driver) GetSSHHostname() (string, error) {
	return d.IPAddress, nil
}

// Return the state of the hyperkit pid
func pidState(pid int) (state.State, error) {
	if pid == 0 {
		return state.Stopped, nil
	}
	p, err := ps.FindProcess(pid)
	if err != nil {
		return state.Error, err
	}
	if p == nil {
		log.Debugf("hyperkit pid %d missing from process table", pid)
		return state.Stopped, nil
	}
	// hyperkit or com.docker.hyper
	if !strings.Contains(p.Executable(), "hyper") {
		log.Debugf("pid %d is stale, and is being used by %s", pid, p.Executable())
		return state.Stopped, nil
	}
	return state.Running, nil
}

// GetState returns the state that the host is in (running, stopped, etc)
func (d *Driver) GetState() (state.State, error) {
	if err := d.verifyRootPermissions(); err != nil {
		return state.Error, err
	}

	pid := d.getPid()
	log.Debugf("hyperkit pid from json: %d", pid)
	return pidState(pid)
}

// Kill stops a host forcefully
func (d *Driver) Kill() error {
	if err := d.verifyRootPermissions(); err != nil {
		return err
	}
	return d.sendSignal(syscall.SIGKILL)
}

// Remove a host
func (d *Driver) Remove() error {
	if err := d.verifyRootPermissions(); err != nil {
		return err
	}

	s, err := d.GetState()
	if err != nil || s == state.Error {
		log.Debugf("Error checking machine status: %v, assuming it has been removed already", err)
	}
	if s == state.Running {
		if err := d.Kill(); err != nil {
			return err
		}
	}
	return nil
}

// Restart a host
func (d *Driver) Restart() error {
	if err := d.Stop(); err != nil {
		return err
	}

	return d.Start()
}

// Start a host
func (d *Driver) Start() error {
	if err := d.verifyRootPermissions(); err != nil {
		return err
	}

	stateDir := d.ResolveStorePath("")
	if err := d.recoverFromUncleanShutdown(); err != nil {
		return err
	}
	h, err := hyperkit.New(d.HyperKitPath, d.VpnKitSock, stateDir)
	if err != nil {
		return errors.Wrap(err, "new-ing Hyperkit")
	}
	log.Debugf("Using hyperkit binary from %s", h.HyperKit)
	// TODO: handle the rest of our settings.
	h.Kernel = d.VmlinuzPath
	h.Initrd = d.InitrdPath
	h.VMNet = d.VMNet
	h.Console = hyperkit.ConsoleFile
	h.CPUs = d.CPU
	h.Memory = d.Memory
	h.UUID = d.UUID
	h.VSock = true
	h.VSockGuestCID = 3

	if vsockPorts, err := d.extractVSockPorts(); err != nil {
		return err
	} else if len(vsockPorts) >= 1 {
		h.VSockPorts = vsockPorts
	}

	mac := ""
	if d.VMNet {
		var err error
		log.Debugf("Using UUID %s", h.UUID)
		mac, err = GetMACAddressFromUUID(h.UUID)
		if err != nil {
			return errors.Wrap(err, "getting MAC address from UUID")
		}

		// Need to strip 0's
		mac = trimMacAddress(mac)
		log.Debugf("Generated MAC %s", mac)
	}

	if d.ImageFormat != "qcow2" {
		return fmt.Errorf("Unsupported VM image format: %s", d.ImageFormat)
	}
	h.Disks = []hyperkit.DiskConfig{
		{
			Path:   fmt.Sprintf("file://%s", d.getDiskPath()),
			Driver: "virtio-blk",
			Format: "qcow",
		},
	}
	log.Debugf("Starting with cmdline: %s", d.Cmdline)
	if err := h.Start(d.Cmdline); err != nil {
		log.Debugf("Error trying to execute %s", h.CmdLine)
		return errors.Wrapf(err, "starting with cmd line: %s", d.Cmdline)
	}

	log.Debugf("Trying to execute %s", h.CmdLine)

	if !d.VMNet {
		return nil
	}

	getIP := func() error {
		st, err := d.GetState()
		if err != nil {
			return errors.Wrap(err, "get state")
		}
		if st == state.Error || st == state.Stopped {
			return fmt.Errorf("hyperkit crashed! command line:\n  hyperkit %s", d.Cmdline)
		}

		d.IPAddress, err = GetIPAddressByMACAddress(mac)
		if err != nil {
			return &RetriableError{Err: err}
		}
		return nil
	}

	if err := RetryAfter(60, getIP, 2*time.Second); err != nil {
		return fmt.Errorf("IP address never found in dhcp leases file %v", err)
	}
	log.Debugf("IP: %s", d.IPAddress)

	return nil
}

// GetURL is not implemented yet
func (d *Driver) GetURL() (string, error) {
	return "", nil
}

func (d *Driver) DriverVersion() string {
	return DriverVersion
}

//recoverFromUncleanShutdown searches for an existing hyperkit.pid file in
//the machine directory. If it can't find it, a clean shutdown is assumed.
//If it finds the pid file, it checks for a running hyperkit process with that pid
//as the existence of a file might not indicate an unclean shutdown but an actual running
//hyperkit server. This is an error situation - we shouldn't start minikube as there is likely
//an instance running already. If the PID in the pidfile does not belong to a running hyperkit
//process, we can safely delete it, and there is a good chance the machine will recover when restarted.
func (d *Driver) recoverFromUncleanShutdown() error {
	pidFile := d.ResolveStorePath(pidFileName)

	if _, err := os.Stat(pidFile); err != nil {
		if os.IsNotExist(err) {
			log.Debugf("clean start, hyperkit pid file doesn't exist: %s", pidFile)
			return nil
		}
		return errors.Wrap(err, "stat")
	}

	log.Warnf("crc might have been shutdown in an unclean way, the hyperkit pid file still exists: %s", pidFile)
	bs, err := ioutil.ReadFile(pidFile)
	if err != nil {
		return errors.Wrapf(err, "reading pidfile %s", pidFile)
	}
	content := strings.TrimSpace(string(bs))
	pid, err := strconv.Atoi(content)
	if err != nil {
		return errors.Wrapf(err, "parsing pidfile %s", pidFile)
	}

	st, err := pidState(pid)
	if err != nil {
		return errors.Wrap(err, "pidState")
	}

	log.Debugf("pid %d is in state %q", pid, st)
	if st == state.Running {
		return nil
	}
	log.Debugf("Removing stale pid file %s...", pidFile)
	if err := os.Remove(pidFile); err != nil {
		return errors.Wrap(err, fmt.Sprintf("removing pidFile %s", pidFile))
	}
	return nil
}

// Stop a host gracefully
func (d *Driver) Stop() error {
	if err := d.verifyRootPermissions(); err != nil {
		return err
	}

	s, err := d.GetState()
	if err != nil {
		return err
	}

	if s != state.Stopped {
		err := d.sendSignal(syscall.SIGTERM)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("hyperkit sigterm failed"))
		}
		// wait 120s for graceful shutdown
		for i := 0; i < 60; i++ {
			time.Sleep(2 * time.Second)
			s, _ := d.GetState()
			log.Debugf("VM state: %s", s)
			if s == state.Stopped {
				return nil
			}
		}
		return errors.New("VM Failed to gracefully shutdown, try the kill command")
	}
	return nil
}

// InvalidPortNumberError implements the Error interface.
// It is used when a VSockPorts port number cannot be recognised as an integer.
type InvalidPortNumberError string

// Error returns an Error for InvalidPortNumberError
func (port InvalidPortNumberError) Error() string {
	return fmt.Sprintf("vsock port '%s' is not an integer", string(port))
}

func (d *Driver) extractVSockPorts() ([]int, error) {
	vsockPorts := make([]int, 0, len(d.VSockPorts))

	for _, port := range d.VSockPorts {
		p, err := strconv.Atoi(port)
		if err != nil {
			return nil, InvalidPortNumberError(port)
		}
		vsockPorts = append(vsockPorts, p)
	}

	return vsockPorts, nil
}

func (d *Driver) sendSignal(s os.Signal) error {
	pid := d.getPid()
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	return proc.Signal(s)
}

func (d *Driver) getPid() int {
	pidPath := d.ResolveStorePath(machineFileName)

	f, err := os.Open(pidPath)
	if err != nil {
		log.Warnf("Error reading pid file: %v", err)
		return 0
	}
	dec := json.NewDecoder(f)
	config := hyperkit.HyperKit{}
	if err := dec.Decode(&config); err != nil {
		log.Warnf("Error decoding pid file: %v", err)
		return 0
	}

	return config.Pid
}
func (d *Driver) UpdateConfigRaw(rawConfig []byte) error {
	var newDriver Driver
	err := json.Unmarshal(rawConfig, &newDriver)
	if err != nil {
		return err
	}

	if newDriver.Memory == d.Memory && newDriver.CPU == d.CPU {
		/* For now only changing memory and CPU is supported/tested.
		 * If none of these changed, we might be trying to change another
		 * value, which is may or may not work, return ErrNotImplemented for now
		 */
		return drivers.ErrNotImplemented
	}
	*d = newDriver

	return nil
}
