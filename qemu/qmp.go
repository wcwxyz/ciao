/*
// Copyright (c) 2016 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
*/

package qemu

import (
	"bufio"
	"container/list"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"context"
)

// QMPLog is a logging interface used by the qemu package to log various
// interesting pieces of information.  Rather than introduce a dependency
// on a given logging package, qemu presents this interface that allows
// clients to provide their own logging type which they can use to
// seamlessly integrate qemu's logs into their own logs.  A QMPLog
// implementation can be specified in the QMPConfig structure.
type QMPLog interface {
	// V returns true if the given argument is less than or equal
	// to the implementation's defined verbosity level.
	V(int32) bool

	// Infof writes informational output to the log.  A newline will be
	// added to the output if one is not provided.
	Infof(string, ...interface{})

	// Warningf writes warning output to the log.  A newline will be
	// added to the output if one is not provided.
	Warningf(string, ...interface{})

	// Errorf writes error output to the log.  A newline will be
	// added to the output if one is not provided.
	Errorf(string, ...interface{})
}

type qmpNullLogger struct{}

func (l qmpNullLogger) V(level int32) bool {
	return false
}

func (l qmpNullLogger) Infof(format string, v ...interface{}) {
}

func (l qmpNullLogger) Warningf(format string, v ...interface{}) {
}

func (l qmpNullLogger) Errorf(format string, v ...interface{}) {
}

// QMPConfig is a configuration structure that can be used to specify a
// logger and a channel to which logs and  QMP events are to be sent.  If
// neither of these fields are specified, or are set to nil, no logs will be
// written and no QMP events will be reported to the client.
type QMPConfig struct {
	// eventCh can be specified by clients who wish to receive QMP
	// events.
	EventCh chan<- QMPEvent

	// logger is used by the qmpStart function and all the go routines
	// it spawns to log information.
	Logger QMPLog
}

type qmpEventFilter struct {
	eventName string
	dataKey   string
	dataValue string
}

// QMPEvent contains a single QMP event, sent on the QMPConfig.EventCh channel.
type QMPEvent struct {
	// The name of the event, e.g., DEVICE_DELETED
	Name string

	// The data associated with the event.  The contents of this map are
	// unprocessed by the qemu package.  It is simply the result of
	// unmarshalling the QMP json event.  Here's an example map
	// map[string]interface{}{
	//	"driver": "virtio-blk-pci",
	//	"drive":  "drive_3437843748734873483",
	// }
	Data map[string]interface{}

	// The event's timestamp converted to a time.Time object.
	Timestamp time.Time
}

type qmpResult struct {
	err  error
	data map[string]interface{}
}

type qmpCommand struct {
	ctx            context.Context
	res            chan qmpResult
	name           string
	args           map[string]interface{}
	filter         *qmpEventFilter
	resultReceived bool
}

// QMP is a structure that contains the internal state used by startQMPLoop and
// the go routines it spwans.  All the contents of this structure are private.
type QMP struct {
	cmdCh          chan qmpCommand
	conn           io.ReadWriteCloser
	cfg            QMPConfig
	connectedCh    chan<- *QMPVersion
	disconnectedCh chan struct{}
}

// QMPVersion contains the version number and the capabailities of a QEMU
// instance, as reported in the QMP greeting message.
type QMPVersion struct {
	Major        int
	Minor        int
	Micro        int
	Capabilities []string
}

func (q *QMP) readLoop(fromVMCh chan<- []byte) {
	scanner := bufio.NewScanner(q.conn)
	for scanner.Scan() {
		line := scanner.Bytes()
		if q.cfg.Logger.V(1) {
			q.cfg.Logger.Infof("%s", string(line))
		}
		fromVMCh <- line
	}
	close(fromVMCh)
}

func (q *QMP) processQMPEvent(cmdQueue *list.List, name interface{}, data interface{},
	timestamp interface{}) {

	strname, ok := name.(string)
	if !ok {
		return
	}

	var eventData map[string]interface{}
	if data != nil {
		eventData, _ = data.(map[string]interface{})
	}

	cmdEl := cmdQueue.Front()
	if cmdEl != nil {
		cmd := cmdEl.Value.(*qmpCommand)
		filter := cmd.filter
		if filter != nil {
			if filter.eventName == strname {
				match := filter.dataKey == ""
				if !match && eventData != nil {
					match = eventData[filter.dataKey] == filter.dataValue
				}
				if match {
					if cmd.resultReceived {
						q.finaliseCommand(cmdEl, cmdQueue, true)
					} else {
						cmd.filter = nil
					}
				}
			}
		}
	}

	if q.cfg.EventCh != nil {
		ev := QMPEvent{
			Name: strname,
			Data: eventData,
		}
		if timestamp != nil {
			timestamp, ok := timestamp.(map[string]interface{})
			if ok {
				seconds, _ := timestamp["seconds"].(float64)
				microseconds, _ := timestamp["microseconds"].(float64)
				ev.Timestamp = time.Unix(int64(seconds), int64(microseconds))
			}
		}

		q.cfg.EventCh <- ev
	}
}

func (q *QMP) finaliseCommand(cmdEl *list.Element, cmdQueue *list.List, succeeded bool) {
	cmd := cmdEl.Value.(*qmpCommand)
	cmdQueue.Remove(cmdEl)
	select {
	case <-cmd.ctx.Done():
	default:
		if succeeded {
			cmd.res <- qmpResult{}
		} else {
			cmd.res <- qmpResult{err: fmt.Errorf("QMP command failed")}
		}
	}
	if cmdQueue.Len() > 0 {
		q.writeNextQMPCommand(cmdQueue)
	}
}

func (q *QMP) processQMPInput(line []byte, cmdQueue *list.List) {
	var vmData map[string]interface{}
	err := json.Unmarshal(line, &vmData)
	if err != nil {
		q.cfg.Logger.Warningf("Unable to decode response [%s] from VM: %v",
			string(line), err)
		return
	}
	if evname, found := vmData["event"]; found {
		q.processQMPEvent(cmdQueue, evname, vmData["data"], vmData["timestamp"])
		return
	}

	_, succeeded := vmData["return"]
	_, failed := vmData["error"]

	if !succeeded && !failed {
		return
	}

	cmdEl := cmdQueue.Front()
	if cmdEl == nil {
		q.cfg.Logger.Warningf("Unexpected command response received [%s] from VM",
			string(line))
		return
	}
	cmd := cmdEl.Value.(*qmpCommand)
	if failed || cmd.filter == nil {
		q.finaliseCommand(cmdEl, cmdQueue, succeeded)
	} else {
		cmd.resultReceived = true
	}
}

func (q *QMP) writeNextQMPCommand(cmdQueue *list.List) {
	cmdEl := cmdQueue.Front()
	cmd := cmdEl.Value.(*qmpCommand)
	cmdData := make(map[string]interface{})
	cmdData["execute"] = cmd.name
	if cmd.args != nil {
		cmdData["arguments"] = cmd.args
	}
	encodedCmd, err := json.Marshal(&cmdData)
	if err != nil {
		cmd.res <- qmpResult{
			err: fmt.Errorf("Unable to marhsall command %s: %v",
				cmd.name, err),
		}
		cmdQueue.Remove(cmdEl)
	}
	q.cfg.Logger.Infof("%s", string(encodedCmd))
	encodedCmd = append(encodedCmd, '\n')
	_, err = q.conn.Write(encodedCmd)
	if err != nil {
		cmd.res <- qmpResult{
			err: fmt.Errorf("Unable to write command to qmp socket %v", err),
		}
		cmdQueue.Remove(cmdEl)
	}
}

func failOutstandingCommands(cmdQueue *list.List) {
	for e := cmdQueue.Front(); e != nil; e = e.Next() {
		cmd := e.Value.(*qmpCommand)
		select {
		case cmd.res <- qmpResult{
			err: errors.New("exitting QMP loop, command cancelled"),
		}:
		case <-cmd.ctx.Done():
		}
	}
}

func (q *QMP) parseVersion(version []byte) *QMPVersion {
	var qmp map[string]interface{}
	err := json.Unmarshal(version, &qmp)
	if err != nil {
		q.cfg.Logger.Errorf("Invalid QMP greeting: %s", string(version))
		return nil
	}

	versionMap := qmp
	for _, k := range []string{"QMP", "version", "qemu"} {
		versionMap, _ = versionMap[k].(map[string]interface{})
		if versionMap == nil {
			q.cfg.Logger.Errorf("Invalid QMP greeting: %s", string(version))
			return nil
		}
	}

	micro, _ := versionMap["micro"].(float64)
	minor, _ := versionMap["minor"].(float64)
	major, _ := versionMap["major"].(float64)
	capabilities, _ := qmp["QMP"].(map[string]interface{})["capabilities"].([]interface{})
	stringcaps := make([]string, 0, len(capabilities))
	for _, c := range capabilities {
		if cap, ok := c.(string); ok {
			stringcaps = append(stringcaps, cap)
		}
	}
	return &QMPVersion{Major: int(major),
		Minor:        int(minor),
		Micro:        int(micro),
		Capabilities: stringcaps,
	}
}

func (q *QMP) mainLoop() {
	cmdQueue := list.New().Init()
	fromVMCh := make(chan []byte)
	go q.readLoop(fromVMCh)

	defer func() {
		if q.cfg.EventCh != nil {
			close(q.cfg.EventCh)
		}
		_ = q.conn.Close()
		_ = <-fromVMCh
		failOutstandingCommands(cmdQueue)
		close(q.disconnectedCh)
	}()

	version := []byte{}

DONE:
	for {
		var ok bool
		select {
		case cmd, ok := <-q.cmdCh:
			if !ok {
				return
			}
			_ = cmdQueue.PushBack(&cmd)
		case version, ok = <-fromVMCh:
			if !ok {
				return
			}
			if cmdQueue.Len() >= 1 {
				q.writeNextQMPCommand(cmdQueue)
			}
			break DONE
		}
	}

	q.connectedCh <- q.parseVersion(version)

	for {
		select {
		case cmd, ok := <-q.cmdCh:
			if !ok {
				return
			}
			_ = cmdQueue.PushBack(&cmd)
			if cmdQueue.Len() >= 1 {
				q.writeNextQMPCommand(cmdQueue)
			}
		case line, ok := <-fromVMCh:
			if !ok {
				return
			}
			q.processQMPInput(line, cmdQueue)
		}
	}
}

func startQMPLoop(conn io.ReadWriteCloser, cfg QMPConfig,
	connectedCh chan<- *QMPVersion, disconnectedCh chan struct{}) *QMP {
	q := &QMP{
		cmdCh:          make(chan qmpCommand),
		conn:           conn,
		cfg:            cfg,
		connectedCh:    connectedCh,
		disconnectedCh: disconnectedCh,
	}
	go q.mainLoop()
	return q
}

func (q *QMP) ExecuteCommand(ctx context.Context, name string, args map[string]interface{},
	filter *qmpEventFilter) error {
	var err error
	resCh := make(chan qmpResult)
	select {
	case <-q.disconnectedCh:
		err = errors.New("exitting QMP loop, command cancelled")
	case q.cmdCh <- qmpCommand{
		ctx:    ctx,
		res:    resCh,
		name:   name,
		args:   args,
		filter: filter,
	}:
	}

	if err != nil {
		return err
	}

	select {
	case res := <-resCh:
		err = res.err
	case <-ctx.Done():
		err = ctx.Err()
	}

	return err
}

// QMPStart connects to a unix domain socket maintained by a QMP instance.  It
// waits to receive the QMP welcome message via the socket and spawns some go
// routines to manage the socket.  The function returns a *QMP which can be
// used by callers to send commands to the QEMU instance or to close the
// socket and all the go routines that have been spawned to monitor it.  A
// *QMPVersion is also returned.  This structure contains the version and
// capabilities information returned by the QEMU instance in its welcome
// message.
//
// socket contains the path to the domain socket. cfg contains some options
// that can be specified by the caller, namely where the qemu package should
// send logs and QMP events.  disconnectedCh is a channel that must be supplied
// by the caller.  It is closed when an error occurs openning or writing to
// or reading from the unix domain socket.  This implies that the QEMU instance
// that opened the socket has closed.
//
// If this function returns without error, callers should call QMP.Shutdown
// when they wish to stop monitoring the QMP instance.  This is not strictly
// necessary if the QEMU instance exits and the disconnectedCh is closed, but
// doing so will not cause any problems.
//
// Commands can be sent to the QEMU instance via the QMP.Execute methods.
// These commands are executed serially, even if the QMP.Execute methods
// are called from different go routines.  The QMP.Execute methods will
// block until they have received a success or failure message from QMP,
// i.e., {"return": {}} or {"error":{}}, and in some cases certain events
// are received.
func QMPStart(ctx context.Context, socket string, cfg QMPConfig, disconnectedCh chan struct{}) (*QMP, *QMPVersion, error) {
	if cfg.Logger == nil {
		cfg.Logger = qmpNullLogger{}
	}
	dialer := net.Dialer{Cancel: ctx.Done()}
	conn, err := dialer.Dial("unix", socket)
	if err != nil {
		cfg.Logger.Warningf("Unable to connect to unix socket (%s): %v", socket, err)
		close(disconnectedCh)
		return nil, nil, err
	}

	connectedCh := make(chan *QMPVersion)

	var version *QMPVersion
	q := startQMPLoop(conn, cfg, connectedCh, disconnectedCh)
	select {
	case <-ctx.Done():
		q.Shutdown()
		<-disconnectedCh
		return nil, nil, fmt.Errorf("Canceled by caller")
	case <-disconnectedCh:
		return nil, nil, fmt.Errorf("Lost connection to VM")
	case version = <-connectedCh:
		if version == nil {
			return nil, nil, fmt.Errorf("Failed to find QMP version information")
		}
	}

	return q, version, nil
}

// Shutdown closes the domain socket used to monitor a QEMU instance and
// terminates all the go routines spawned by QMPStart to manage that instance.
// QMP.Shutdown does not shut down the running instance.  Calling QMP.Shutdown
// will result in the disconnectedCh channel being closed, indicating that we
// have lost connection to the QMP instance.  In this case it does not indicate
// that the instance has quit.
//
// QMP.Shutdown should not be called concurrently with other QMP methods.  It
// should not be called twice on the same QMP instance.
//
// Calling QMP.Shutdown after the disconnectedCh channel is closed is permitted but
// will not have any effect.
func (q *QMP) Shutdown() {
	close(q.cmdCh)
}

// ExecuteQMPCapabilities executes the qmp_capabilities command on the instance.
func (q *QMP) ExecuteQMPCapabilities(ctx context.Context) error {
	return q.ExecuteCommand(ctx, "qmp_capabilities", nil, nil)
}

// ExecuteStop sends the stop command to the instance.
func (q *QMP) ExecuteStop(ctx context.Context) error {
	return q.ExecuteCommand(ctx, "stop", nil, nil)
}

// ExecuteCont sends the cont command to the instance.
func (q *QMP) ExecuteCont(ctx context.Context) error {
	return q.ExecuteCommand(ctx, "cont", nil, nil)
}

// ExecuteSystemPowerdown sends the system_powerdown command to the instance.
// This function will block until the SHUTDOWN event is received.
func (q *QMP) ExecuteSystemPowerdown(ctx context.Context) error {
	filter := &qmpEventFilter{
		eventName: "SHUTDOWN",
	}
	return q.ExecuteCommand(ctx, "system_powerdown", nil, filter)
}

// ExecuteQuit sends the quit command to the instance, terminating
// the QMP instance immediately.
func (q *QMP) ExecuteQuit(ctx context.Context) error {
	return q.ExecuteCommand(ctx, "quit", nil, nil)
}

// ExecuteBlockdevAdd sends a blockdev-add to the QEMU instance.  device is the
// path of the device to add, e.g., /dev/rdb0, and blockdevID is an identifier
// used to name the device.  As this identifier will be passed directly to QMP,
// it must obey QMP's naming rules, e,g., it must start with a letter.
func (q *QMP) ExecuteBlockdevAdd(ctx context.Context, device, blockdevID string) error {
	args := map[string]interface{}{
		"options": map[string]interface{}{
			"driver": "raw",
			"file": map[string]interface{}{
				"driver":   "file",
				"filename": device,
			},
			"id": blockdevID,
		},
	}
	return q.ExecuteCommand(ctx, "blockdev-add", args, nil)
}

// ExecuteDeviceAdd adds the guest portion of a device to a QEMU instance
// using the device_add command.  blockdevID should match the blockdevID passed
// to a previous call to ExecuteBlockdevAdd.  devID is the id of the device to
// add.  Both strings must be valid QMP identifiers.  driver is the name of the
// driver,e.g., virtio-blk-pci, and bus is the name of the bus.  bus is optional.
func (q *QMP) ExecuteDeviceAdd(ctx context.Context, blockdevID, devID, driver, bus string) error {
	args := map[string]interface{}{
		"id":     devID,
		"driver": driver,
		"drive":  blockdevID,
	}
	if bus != "" {
		args["bus"] = bus
	}
	return q.ExecuteCommand(ctx, "device_add", args, nil)
}

// ExecuteXBlockdevDel deletes a block device by sending a x-blockdev-del command.
// blockdevID is the id of the block device to be deleted.  Typically, this will
// match the id passed to ExecuteBlockdevAdd.  It must be a valid QMP id.
func (q *QMP) ExecuteXBlockdevDel(ctx context.Context, blockdevID string) error {
	args := map[string]interface{}{
		"id": blockdevID,
	}
	return q.ExecuteCommand(ctx, "x-blockdev-del", args, nil)
}

// ExecuteDeviceDel deletes guest portion of a QEMU device by sending a
// device_del command.   devId is the identifier of the device to delete.
// Typically it would match the devID parameter passed to an earlier call
// to ExecuteDeviceAdd.  It must be a valid QMP identidier.
//
// This method blocks until a DEVICE_DELETED event is received for devID.
func (q *QMP) ExecuteDeviceDel(ctx context.Context, devID string) error {
	args := map[string]interface{}{
		"id": devID,
	}
	filter := &qmpEventFilter{
		eventName: "DEVICE_DELETED",
		dataKey:   "device",
		dataValue: devID,
	}
	return q.ExecuteCommand(ctx, "device_del", args, filter)
}
