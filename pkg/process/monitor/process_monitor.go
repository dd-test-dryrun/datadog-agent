// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux

// Package monitor represents a wrapper to netlink, which gives us the ability to monitor process events like Exec and
// Exit, and activate the registered callbacks for the relevant events
package monitor

import (
	"fmt"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"go.uber.org/atomic"

	"github.com/DataDog/datadog-agent/pkg/eventmonitor/consumers"
	"github.com/DataDog/datadog-agent/pkg/network/protocols/telemetry"
	"github.com/DataDog/datadog-agent/pkg/runtime"
	"github.com/DataDog/datadog-agent/pkg/util/kernel"
	"github.com/DataDog/datadog-agent/pkg/util/kernel/netns"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const (
	// The size of the process events queue of netlink.
	processMonitorEventQueueSize = 2048
	// The size of the callbacks queue for pending tasks.
	pendingCallbacksQueueSize = 5000
)

var (
	processMonitor = &ProcessMonitor{
		// Must initialize the sets, as we can register callbacks prior to calling Initialize.
		processExecCallbacks: make(map[*ProcessCallback]struct{}, 0),
		processExitCallbacks: make(map[*ProcessCallback]struct{}, 0),
		oversizedLogLimit:    log.NewLogLimit(10, 10*time.Minute),
	}
)

// processMonitorTelemetry
type processMonitorTelemetry struct {
	mg *telemetry.MetricGroup
	// process monitor will process :
	//  o events refer to netlink events received (not only exec and exit)
	//  o exec process netlink events
	//  o exit process netlink events
	//  o restart the netlink connection
	//
	//  o reinit_failed is the number of failed re-initialisation after a netlink restart due to an error
	//  o process_scan_failed would be > 0 if initial process scan failed
	//  o callback_called numbers of callback called
	events  *telemetry.Counter
	exec    *telemetry.Counter
	exit    *telemetry.Counter
	restart *telemetry.Counter

	reinitFailed      *telemetry.Counter
	processScanFailed *telemetry.Counter
	callbackExecuted  *telemetry.Counter

	processExecChannelIsFull *telemetry.Counter
	processExitChannelIsFull *telemetry.Counter
}

func newProcessMonitorTelemetry() processMonitorTelemetry {
	metricGroup := telemetry.NewMetricGroup(
		"usm.process.monitor",
		telemetry.OptPrometheus,
	)
	return processMonitorTelemetry{
		mg:      metricGroup,
		events:  metricGroup.NewCounter("events"),
		exec:    metricGroup.NewCounter("exec"),
		exit:    metricGroup.NewCounter("exit"),
		restart: metricGroup.NewCounter("restart"),

		reinitFailed:      metricGroup.NewCounter("reinit_failed"),
		processScanFailed: metricGroup.NewCounter("process_scan_failed"),
		callbackExecuted:  metricGroup.NewCounter("callback_executed"),

		processExecChannelIsFull: metricGroup.NewCounter("process_exec_channel_is_full"),
		processExitChannelIsFull: metricGroup.NewCounter("process_exit_channel_is_full"),
	}
}

// ProcessMonitor uses netlink process events like Exec and Exit and activate the registered callbacks for the relevant
// events.
// ProcessMonitor require root or CAP_NET_ADMIN capabilities
type ProcessMonitor struct {
	initOnce sync.Once
	// A wait group to give the Stop method an option to wait until the main event loop finished its teardown.
	processMonitorWG sync.WaitGroup
	// A wait group to give us the option to wait until all callback runners have finished.
	callbackRunnersWG sync.WaitGroup
	// An atomic counter to know how much instances do we have in any given time. Used to ensure when to clean up all
	// resources.
	refcount atomic.Int32
	// A channel to mark the main routines to halt.
	done chan struct{}

	// netlink channels for process event monitor.
	netlinkEventsChannel chan netlink.ProcEvent
	netlinkDoneChannel   chan struct{}
	netlinkErrorsChannel chan error

	useEventStream bool

	// callback registration and parallel execution management

	hasExecCallbacks          atomic.Bool
	processExecCallbacksMutex sync.RWMutex
	processExecCallbacks      map[*ProcessCallback]struct{}

	hasExitCallbacks          atomic.Bool
	processExitCallbacksMutex sync.RWMutex
	processExitCallbacks      map[*ProcessCallback]struct{}

	// The callbackRunnerStopChannel is used to signal the callback runners to stop
	callbackRunnerStopChannel chan struct{}
	// The callbackRunner is used to send tasks to the callback runners
	callbackRunner chan func()

	tel processMonitorTelemetry

	oversizedLogLimit *log.Limit
}

// ProcessCallback is a callback function that is called on a given pid that represents a new process.
type ProcessCallback = func(pid uint32)

// GetProcessMonitor create a monitor (only once) that register to netlink process events.
//
// This monitor can monitor.Subscribe(callback, filter) callback on particular event
// like process EXEC, EXIT. The callback will be called when the filter will match.
// Filter can be applied on :
//
//	process name (NAME)
//	by default ANY is applied
//
// Typical initialization:
//
//	mon := GetProcessMonitor()
//	mon.Subscribe(callback)
//	mon.Initialize()
//
// note: o GetProcessMonitor() will always return the same instance
//
//	  as we can only register once with netlink process event
//	o mon.Subscribe() will subscribe callback before or after the Initialization
//	o mon.Initialize() will scan current processes and call subscribed callback
//
//	o callback{Event: EXIT, Metadata: ANY}   callback is called for all exit events (system-wide)
//	o callback{Event: EXIT, Metadata: NAME}  callback will be called if we have seen the process Exec event,
//	                                         the metadata will be saved between Exec and Exit event per pid
//	                                         then the Exit callback will evaluate the same metadata on Exit.
//	                                         We need to save the metadata here as /proc/pid doesn't exist anymore.
func GetProcessMonitor() *ProcessMonitor {
	processMonitor.refcount.Inc()
	return processMonitor
}

// handleProcessExec is a callback function called on a given pid that represents a new process.
// we're iterating the relevant callbacks and trigger them.
func (pm *ProcessMonitor) handleProcessExec(pid uint32) {
	pm.processExecCallbacksMutex.RLock()
	defer pm.processExecCallbacksMutex.RUnlock()

	for callback := range pm.processExecCallbacks {
		temporaryCallback := callback
		select {
		case pm.callbackRunner <- func() { (*temporaryCallback)(pid) }:
			continue
		default:
			pm.tel.processExecChannelIsFull.Add(1)
			if log.ShouldLog(log.DebugLvl) && pm.oversizedLogLimit.ShouldLog() {
				log.Debug("can't send exec callback to callbackRunner, channel is full")
			}
		}
	}
}

// handleProcessExit is a callback function called on a given pid that represents an exit event.
// we're iterating the relevant callbacks and trigger them.
func (pm *ProcessMonitor) handleProcessExit(pid uint32) {
	pm.processExitCallbacksMutex.RLock()
	defer pm.processExitCallbacksMutex.RUnlock()

	for callback := range pm.processExitCallbacks {
		temporaryCallback := callback
		select {
		case pm.callbackRunner <- func() { (*temporaryCallback)(pid) }:
			continue
		default:
			pm.tel.processExitChannelIsFull.Add(1)
			if log.ShouldLog(log.DebugLvl) && pm.oversizedLogLimit.ShouldLog() {
				log.Debug("can't send exit callback to callbackRunner, channel is full")
			}
		}
	}
}

// initNetlinkProcessEventMonitor initialize the netlink socket filter for process event monitor.
func (pm *ProcessMonitor) initNetlinkProcessEventMonitor() error {
	pm.netlinkDoneChannel = make(chan struct{})
	pm.netlinkErrorsChannel = make(chan error, 10)
	pm.netlinkEventsChannel = make(chan netlink.ProcEvent, processMonitorEventQueueSize)

	if err := netns.WithRootNS(kernel.ProcFSRoot(), func() error {
		return netlink.ProcEventMonitor(pm.netlinkEventsChannel, pm.netlinkDoneChannel, pm.netlinkErrorsChannel, netlink.PROC_EVENT_EXEC|netlink.PROC_EVENT_EXIT)
	}); err != nil {
		return fmt.Errorf("couldn't initialize process monitor: %s", err)
	}

	return nil
}

// initCallbackRunner runs multiple workers that run tasks sent over a queue.
func (pm *ProcessMonitor) initCallbackRunner() {
	cpuNum, err := kernel.PossibleCPUs()
	if err != nil {
		cpuNum = runtime.NumVCPU()
	}
	pm.callbackRunner = make(chan func(), pendingCallbacksQueueSize)
	pm.callbackRunnerStopChannel = make(chan struct{})
	pm.callbackRunnersWG.Add(cpuNum)
	for i := 0; i < cpuNum; i++ {
		// Copy i to avoid unexpected behaviors
		callbackRunnerIndex := i
		go func() {
			defer pm.callbackRunnersWG.Done()
			for {
				// We utilize the callbackRunnerStopChannel to signal the stopping point,
				// as closing the callbackRunner channel could lead to a panic when attempting to write to it.

				// Trying to exit the goroutine as early as possible.
				// This is essential because of how the Go select statement functions. if both cases evaluate to true, it will randomly choose between the two.

				// In other words, when only the second select statement is present, and we set the callbackRunnerStopChannel, there's a 50% chance that the second case
				// will be chosen due to the workings of the select mechanism in Go. This is why we introduced the first select statement,
				// to attempt early termination of the goroutine (drawing inspiration from https://go101.org/article/channel-closing.html)
				select {
				case <-pm.callbackRunnerStopChannel:
					log.Debugf("callback runner %d has completed its execution", callbackRunnerIndex)
					return
				default:
				}

				select {
				case <-pm.callbackRunnerStopChannel:
					log.Debugf("callback runner %d has completed its execution", callbackRunnerIndex)
					return
				case call := <-pm.callbackRunner:
					if call != nil {
						pm.tel.callbackExecuted.Add(1)
						call()
					}
				}
			}
		}()
	}
}

func (pm *ProcessMonitor) stopCallbackRunners() {
	close(pm.callbackRunnerStopChannel)
	pm.callbackRunnersWG.Wait()
}

// mainEventLoop is an event loop receiving events from netlink, or periodic events, and handles them.
func (pm *ProcessMonitor) mainEventLoop() {
	log.Info("process monitor main event loop is starting")
	logTicker := time.NewTicker(2 * time.Minute)

	defer func() {
		logTicker.Stop()
		// Marking netlink to stop, so we won't get any new events.
		close(pm.netlinkDoneChannel)

		pm.stopCallbackRunners()

		// We intentionally don't close the callbackRunner channel,
		// as we don't want to panic if we're trying to send to it in another goroutine.

		// Before shutting down, making sure we're cleaning all resources.
		pm.processMonitorWG.Done()
	}()

	maxChannelSize := 0
	for {
		select {
		case <-pm.done:
			log.Info("process monitor main event loop is shutting down, having been marked to stop")
			return
		case event, ok := <-pm.netlinkEventsChannel:
			if !ok {
				log.Info("process monitor main event loop is shutting down, netlink events channel was closed")
				return
			}

			if maxChannelSize < len(pm.netlinkEventsChannel) {
				maxChannelSize = len(pm.netlinkEventsChannel)
			}

			pm.tel.events.Add(1)
			switch ev := event.Msg.(type) {
			case *netlink.ExecProcEvent:
				pm.tel.exec.Add(1)
				// handleProcessExec locks a mutex to access the exec callbacks array, if it is empty, then we're
				// wasting "resources" to check it. Since it is a hot-code-path, it has some cpu load.
				// Checking an atomic boolean, is an atomic operation, hence much faster.
				if pm.hasExecCallbacks.Load() {
					pm.handleProcessExec(ev.ProcessPid)
				}
			case *netlink.ExitProcEvent:
				pm.tel.exit.Add(1)
				// handleProcessExit locks a mutex to access the exit callbacks array, if it is empty, then we're
				// wasting "resources" to check it. Since it is a hot-code-path, it has some cpu load.
				// Checking an atomic boolean, is an atomic operation, hence much faster.
				if pm.hasExitCallbacks.Load() {
					pm.handleProcessExit(ev.ProcessPid)
				}
			}
		case _, ok := <-pm.netlinkErrorsChannel:
			if !ok {
				log.Info("process monitor main event loop is shutting down, netlink errors channel was closed")
				return
			}
			pm.tel.restart.Add(1)

			pm.netlinkDoneChannel <- struct{}{}
			// Netlink might suffer from temporary errors (insufficient buffer for example). We're trying to recover
			// by reinitializing netlink socket.
			// Waiting a bit before reinitializing.
			time.Sleep(50 * time.Millisecond)
			if err := pm.initNetlinkProcessEventMonitor(); err != nil {
				log.Errorf("failed re-initializing process monitor: %s", err)
				pm.tel.reinitFailed.Add(1)
				return
			}
		case <-logTicker.C:
			log.Debugf("process monitor stats - %s; max channel size: %d / 2 minutes)",
				pm.tel.mg.Summary(),
				maxChannelSize,
			)
			maxChannelSize = 0
		}
	}
}

// Initialize setting up the process monitor only once, no matter how many times it was called.
// The initialization order:
//  1. Initializes callback workers.
//  2. Initializes the netlink process monitor.
//  2. Run the main event loop in a goroutine.
//  4. Scans already running processes and call the Exec callbacks on them.
func (pm *ProcessMonitor) Initialize(useEventStream bool) error {
	var initErr error
	pm.initOnce.Do(
		func() {
			method := "netlink"
			if useEventStream {
				method = "event stream"
			}
			log.Infof("initializing process monitor (%s)", method)
			pm.tel = newProcessMonitorTelemetry()

			pm.useEventStream = useEventStream
			pm.done = make(chan struct{})
			pm.initCallbackRunner()

			if useEventStream {
				return
			}

			pm.processMonitorWG.Add(1)
			// Setting up the main loop
			pm.netlinkDoneChannel = make(chan struct{})
			pm.netlinkErrorsChannel = make(chan error, 10)
			pm.netlinkEventsChannel = make(chan netlink.ProcEvent, processMonitorEventQueueSize)

			go pm.mainEventLoop()

			if err := netns.WithRootNS(kernel.ProcFSRoot(), func() error {
				return netlink.ProcEventMonitor(pm.netlinkEventsChannel, pm.netlinkDoneChannel, pm.netlinkErrorsChannel, netlink.PROC_EVENT_EXEC|netlink.PROC_EVENT_EXIT)
			}); err != nil {
				initErr = fmt.Errorf("couldn't initialize process monitor: %w", err)
			}

			pm.processExecCallbacksMutex.RLock()
			execCallbacksLength := len(pm.processExecCallbacks)
			pm.processExecCallbacksMutex.RUnlock()

			// Initialize should be called only once after we registered all callbacks. Thus, if we have no registered
			// callback, no need to scan already running processes.
			if execCallbacksLength > 0 {
				handleProcessExecWrapper := func(pid int) error {
					pm.handleProcessExec(uint32(pid))
					return nil
				}
				// Scanning already running processes
				log.Info("process monitor init, scanning all processes")
				if err := kernel.WithAllProcs(kernel.ProcFSRoot(), handleProcessExecWrapper); err != nil {
					initErr = fmt.Errorf("process monitor init, scanning all process failed %s", err)
					pm.tel.processScanFailed.Add(1)
					return
				}
			}
		},
	)
	return initErr
}

// SubscribeExec register an exec callback and returns unsubscribe function callback that removes the callback.
//
// A callback can be registered only once, callback with a filter type (not ANY) must be registered before the matching
// Exit callback.
func (pm *ProcessMonitor) SubscribeExec(callback ProcessCallback) func() {
	pm.processExecCallbacksMutex.Lock()
	pm.hasExecCallbacks.Store(true)
	pm.processExecCallbacks[&callback] = struct{}{}
	pm.processExecCallbacksMutex.Unlock()

	// UnSubscribe()
	return func() {
		pm.processExecCallbacksMutex.Lock()
		delete(pm.processExecCallbacks, &callback)
		pm.hasExecCallbacks.Store(len(pm.processExecCallbacks) > 0)
		pm.processExecCallbacksMutex.Unlock()
	}
}

// SubscribeExit register an exit callback and returns unsubscribe function callback that removes the callback.
func (pm *ProcessMonitor) SubscribeExit(callback ProcessCallback) func() {
	pm.processExitCallbacksMutex.Lock()
	pm.hasExitCallbacks.Store(true)
	pm.processExitCallbacks[&callback] = struct{}{}
	pm.processExitCallbacksMutex.Unlock()

	// UnSubscribe()
	return func() {
		pm.processExitCallbacksMutex.Lock()
		delete(pm.processExitCallbacks, &callback)
		pm.hasExitCallbacks.Store(len(pm.processExitCallbacks) > 0)
		pm.processExitCallbacksMutex.Unlock()
	}
}

// Stop decreasing the refcount, and if we reach 0 we terminate the main event loop.
func (pm *ProcessMonitor) Stop() {
	if pm.refcount.Dec() != 0 {
		if pm.refcount.Load() < 0 {
			pm.refcount.Swap(0)
		}
		return
	}

	// We can get here only once, if the refcount is zero.
	log.Info("process monitor stopping due to a refcount of 0")
	if pm.done != nil {
		close(pm.done)

		if pm.useEventStream {
			// For the netlink case, the callback runners are waited for by the
			// main event loop which we wait for with `processMonitorWG` below.
			// However, for the event stream case, we don't have that event
			// loop, so wait here for the callback runners.
			pm.stopCallbackRunners()
		} else {
			pm.processMonitorWG.Wait()
		}

		pm.done = nil
	}

	// that's being done for testing purposes.
	// As tests are running altogether, initOne and processMonitor are being created only once per compilation unit
	// thus, the first test works without an issue, but the second test has troubles.
	pm.processMonitorWG = sync.WaitGroup{}
	pm.callbackRunnersWG = sync.WaitGroup{}
	pm.initOnce = sync.Once{}
	pm.processExecCallbacksMutex.Lock()
	pm.processExecCallbacks = make(map[*ProcessCallback]struct{})
	pm.processExecCallbacksMutex.Unlock()
	pm.processExitCallbacksMutex.Lock()
	pm.processExitCallbacks = make(map[*ProcessCallback]struct{})
	pm.processExitCallbacksMutex.Unlock()
}

// InitializeEventConsumer initializes the event consumer with the event handling.
func InitializeEventConsumer(consumer *consumers.ProcessConsumer) {
	consumer.SubscribeExec(func(pid uint32) {
		processMonitor.tel.events.Add(1)
		processMonitor.tel.exec.Add(1)
		if processMonitor.hasExecCallbacks.Load() {
			processMonitor.handleProcessExec(pid)
		}
	})
	consumer.SubscribeExit(func(pid uint32) {
		processMonitor.tel.events.Add(1)
		processMonitor.tel.exit.Add(1)
		if processMonitor.hasExitCallbacks.Load() {
			processMonitor.handleProcessExit(pid)
		}
	})
}
