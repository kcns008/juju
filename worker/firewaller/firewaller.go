// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package firewaller

import (
	"strings"

	"github.com/juju/errors"
	"github.com/juju/names"

	"github.com/juju/juju/api/firewaller"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/network"
	"github.com/juju/juju/watcher"
	"github.com/juju/juju/worker"
	"github.com/juju/juju/worker/catacomb"
	"github.com/juju/juju/worker/environ"
)

type machineRanges map[network.PortRange]bool

// Firewaller watches the state for port ranges opened or closed on
// machines and reflects those changes onto the backing environment.
// Uses Firewaller API V1.
type Firewaller struct {
	catacomb        catacomb.Catacomb
	st              *firewaller.State
	environ         environs.Environ
	environWatcher  watcher.NotifyWatcher
	machinesWatcher watcher.StringsWatcher
	portsWatcher    watcher.StringsWatcher
	machineds       map[names.MachineTag]*machineData
	unitsChange     chan *unitsChange
	unitds          map[names.UnitTag]*unitData
	serviceds       map[names.ServiceTag]*serviceData
	exposedChange   chan *exposedChange
	globalMode      bool
	globalPortRef   map[network.PortRange]int
	machinePorts    map[names.MachineTag]machineRanges
}

// NewFirewaller returns a new Firewaller or a new FirewallerV0,
// depending on what the API supports.
func NewFirewaller(st *firewaller.State) (worker.Worker, error) {
	fw := &Firewaller{
		st:            st,
		machineds:     make(map[names.MachineTag]*machineData),
		unitsChange:   make(chan *unitsChange),
		unitds:        make(map[names.UnitTag]*unitData),
		serviceds:     make(map[names.ServiceTag]*serviceData),
		exposedChange: make(chan *exposedChange),
		machinePorts:  make(map[names.MachineTag]machineRanges),
	}
	err := catacomb.Invoke(catacomb.Plan{
		Site: &fw.catacomb,
		Work: fw.loop,
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	return fw, nil
}

func (fw *Firewaller) setUp() error {
	var err error
	fw.environWatcher, err = fw.st.WatchForEnvironConfigChanges()
	if err != nil {
		return errors.Trace(err)
	}
	if err := fw.catacomb.Add(fw.environWatcher); err != nil {
		return errors.Trace(err)
	}

	// We won't "wait" actually, because the environ is already
	// available and has a guaranteed valid config, but until
	// WaitForEnviron goes away, this code needs to stay.
	fw.environ, err = environ.WaitForEnviron(fw.environWatcher, fw.st, fw.catacomb.Dying())
	if err != nil {
		if err == environ.ErrWaitAborted {
			return fw.catacomb.ErrDying()
		}
		return errors.Trace(err)
	}
	switch fw.environ.Config().FirewallMode() {
	case config.FwGlobal:
		fw.globalMode = true
		fw.globalPortRef = make(map[network.PortRange]int)
	case config.FwNone:
		logger.Warningf("stopping firewaller - firewall-mode is %q", config.FwNone)
		// XXX(fwereade): shouldn't this be nil? Nothing wrong, nothing to do,
		// now that we've logged there's no further reason to complain or retry.
		return errors.Errorf("firewaller is disabled when firewall-mode is %q", config.FwNone)
	}

	fw.machinesWatcher, err = fw.st.WatchEnvironMachines()
	if err != nil {
		return errors.Trace(err)
	}
	if err := fw.catacomb.Add(fw.machinesWatcher); err != nil {
		return errors.Trace(err)
	}

	fw.portsWatcher, err = fw.st.WatchOpenedPorts()
	if err != nil {
		return errors.Annotatef(err, "failed to start ports watcher")
	}
	if err := fw.catacomb.Add(fw.portsWatcher); err != nil {
		return errors.Trace(err)
	}

	logger.Debugf("started watching opened port ranges for the environment")
	return nil
}

func (fw *Firewaller) loop() error {
	if err := fw.setUp(); err != nil {
		return errors.Trace(err)
	}
	var reconciled bool
	portsChange := fw.portsWatcher.Changes()
	for {
		select {
		case <-fw.catacomb.Dying():
			return fw.catacomb.ErrDying()
		case _, ok := <-fw.environWatcher.Changes():
			logger.Debugf("got environ config changes")
			if !ok {
				return errors.New("environment configuration watcher closed")
			}
			config, err := fw.st.EnvironConfig()
			if err != nil {
				return errors.Trace(err)
			}
			if err := fw.environ.SetConfig(config); err != nil {
				// XXX(fwereade): surely this is an error? probably moot, will
				// hopefully be replaced with EnvironObserver.
				logger.Errorf("loaded invalid environment configuration: %v", err)
			}
		case change, ok := <-fw.machinesWatcher.Changes():
			if !ok {
				return errors.New("machines watcher closed")
			}
			for _, machineId := range change {
				if err := fw.machineLifeChanged(names.NewMachineTag(machineId)); err != nil {
					return err
				}
			}
			if !reconciled {
				reconciled = true
				var err error
				if fw.globalMode {
					err = fw.reconcileGlobal()
				} else {
					err = fw.reconcileInstances()
				}
				if err != nil {
					return errors.Trace(err)
				}
			}
		case change, ok := <-portsChange:
			if !ok {
				return errors.New("ports watcher closed")
			}
			for _, portsGlobalKey := range change {
				machineTag, networkTag, err := parsePortsKey(portsGlobalKey)
				if err != nil {
					return errors.Trace(err)
				}
				if err := fw.openedPortsChanged(machineTag, networkTag); err != nil {
					return errors.Trace(err)
				}
			}
		case change := <-fw.unitsChange:
			if err := fw.unitsChanged(change); err != nil {
				return errors.Trace(err)
			}
		case change := <-fw.exposedChange:
			change.serviced.exposed = change.exposed
			unitds := []*unitData{}
			for _, unitd := range change.serviced.unitds {
				unitds = append(unitds, unitd)
			}
			if err := fw.flushUnits(unitds); err != nil {
				return errors.Annotate(err, "cannot change firewall ports")
			}
		}
	}
}

// startMachine creates a new data value for tracking details of the
// machine and starts watching the machine for units added or removed.
func (fw *Firewaller) startMachine(tag names.MachineTag) error {
	machined := &machineData{
		fw:           fw,
		tag:          tag,
		unitds:       make(map[names.UnitTag]*unitData),
		openedPorts:  make([]network.PortRange, 0),
		definedPorts: make(map[network.PortRange]names.UnitTag),
	}
	m, err := machined.machine()
	if params.IsCodeNotFound(err) {
		return nil
	} else if err != nil {
		return errors.Annotate(err, "cannot watch machine units")
	}
	unitw, err := m.WatchUnits()
	if err != nil {
		return errors.Trace(err)
	}
	// XXX(fwereade): this is the best of a bunch of bad options. We've started
	// the watch, so we're responsible for it; but we (probably?) need to do this
	// little dance below to update the machined data on the fw loop goroutine,
	// whence it's usually accessed, before we start the machined watchLoop
	// below. That catacomb *should* be the only one responsible -- and it *is*
	// responsible -- but having it in the main fw catacomb as well does no harm,
	// and greatly simplifies the code below (which would otherwise have to
	// manage unitw lifetime and errors manually).
	if err := fw.catacomb.Add(unitw); err != nil {
		return errors.Trace(err)
	}
	select {
	case <-fw.catacomb.Dying():
		return fw.catacomb.ErrDying()
	case change, ok := <-unitw.Changes():
		if !ok {
			return errors.New("machine units watcher closed")
		}
		fw.machineds[tag] = machined
		err = fw.unitsChanged(&unitsChange{machined, change})
		if err != nil {
			delete(fw.machineds, tag)
			return errors.Annotatef(err, "cannot respond to units changes for %q", tag)
		}
	}

	err = catacomb.Invoke(catacomb.Plan{
		Site: &machined.catacomb,
		Work: func() error {
			return machined.watchLoop(unitw)
		},
	})
	if err != nil {
		delete(fw.machineds, tag)
		return errors.Trace(err)
	}
	return nil
}

// startUnit creates a new data value for tracking details of the unit
// The provided machineTag must be the tag for the machine the unit was last
// observed to be assigned to.
func (fw *Firewaller) startUnit(unit *firewaller.Unit, machineTag names.MachineTag) error {
	service, err := unit.Service()
	if err != nil {
		return err
	}
	serviceTag := service.Tag()
	unitTag := unit.Tag()
	if err != nil {
		return err
	}
	unitd := &unitData{
		fw:   fw,
		unit: unit,
		tag:  unitTag,
	}
	fw.unitds[unitTag] = unitd

	unitd.machined = fw.machineds[machineTag]
	unitd.machined.unitds[unitTag] = unitd
	if fw.serviceds[serviceTag] == nil {
		err := fw.startService(service)
		if err != nil {
			delete(fw.unitds, unitTag)
			return err
		}
	}
	unitd.serviced = fw.serviceds[serviceTag]
	unitd.serviced.unitds[unitTag] = unitd

	m, err := unitd.machined.machine()
	if err != nil {
		return err
	}

	// check if the machine has ports open on any networks
	networkTags, err := m.ActiveNetworks()
	if err != nil {
		return errors.Annotatef(err, "failed getting %q active networks", machineTag)
	}
	for _, networkTag := range networkTags {
		err := fw.openedPortsChanged(machineTag, networkTag)
		if err != nil {
			return err
		}
	}

	return nil
}

// startService creates a new data value for tracking details of the
// service and starts watching the service for exposure changes.
func (fw *Firewaller) startService(service *firewaller.Service) error {
	exposed, err := service.IsExposed()
	if err != nil {
		return err
	}
	serviced := &serviceData{
		fw:      fw,
		service: service,
		exposed: exposed,
		unitds:  make(map[names.UnitTag]*unitData),
	}
	err = catacomb.Invoke(catacomb.Plan{
		Site: &serviced.catacomb,
		Work: func() error {
			return serviced.watchLoop(exposed)
		},
	})
	if err != nil {
		return errors.Trace(err)
	}
	if err := fw.catacomb.Add(serviced); err != nil {
		return errors.Trace(err)
	}
	fw.serviceds[service.Tag()] = serviced
	return nil
}

// reconcileGlobal compares the initially started watcher for machines,
// units and services with the opened and closed ports globally and
// opens and closes the appropriate ports for the whole environment.
func (fw *Firewaller) reconcileGlobal() error {
	initialPortRanges, err := fw.environ.Ports()
	if err != nil {
		return err
	}
	collector := make(map[network.PortRange]bool)
	for _, machined := range fw.machineds {
		for portRange, unitTag := range machined.definedPorts {
			unitd, known := machined.unitds[unitTag]
			if !known {
				delete(machined.unitds, unitTag)
				continue
			}
			if unitd.serviced.exposed {
				collector[portRange] = true
			}
		}
	}
	wantedPorts := []network.PortRange{}
	for port := range collector {
		wantedPorts = append(wantedPorts, port)
	}
	// Check which ports to open or to close.
	toOpen := diffRanges(wantedPorts, initialPortRanges)
	toClose := diffRanges(initialPortRanges, wantedPorts)
	if len(toOpen) > 0 {
		logger.Infof("opening global ports %v", toOpen)
		if err := fw.environ.OpenPorts(toOpen); err != nil {
			return err
		}
		network.SortPortRanges(toOpen)
	}
	if len(toClose) > 0 {
		logger.Infof("closing global ports %v", toClose)
		if err := fw.environ.ClosePorts(toClose); err != nil {
			return err
		}
		network.SortPortRanges(toClose)
	}
	return nil
}

// reconcileInstances compares the initially started watcher for machines,
// units and services with the opened and closed ports of the instances and
// opens and closes the appropriate ports for each instance.
func (fw *Firewaller) reconcileInstances() error {
	for _, machined := range fw.machineds {
		m, err := machined.machine()
		if params.IsCodeNotFound(err) {
			if err := fw.forgetMachine(machined); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}
		instanceId, err := m.InstanceId()
		if errors.IsNotProvisioned(err) {
			logger.Warningf("Machine not yet provisioned: %v", err)
			continue
		}
		if err != nil {
			return err
		}
		instances, err := fw.environ.Instances([]instance.Id{instanceId})
		if err == environs.ErrNoInstances {
			return nil
		} else if err != nil {
			return err
		}
		machineId := machined.tag.Id()
		initialPortRanges, err := instances[0].Ports(machineId)
		if err != nil {
			return err
		}

		// Check which ports to open or to close.
		toOpen := diffRanges(machined.openedPorts, initialPortRanges)
		toClose := diffRanges(initialPortRanges, machined.openedPorts)
		if len(toOpen) > 0 {
			logger.Infof("opening instance port ranges %v for %q",
				toOpen, machined.tag)
			if err := instances[0].OpenPorts(machineId, toOpen); err != nil {
				// TODO(mue) Add local retry logic.
				return err
			}
			network.SortPortRanges(toOpen)
		}
		if len(toClose) > 0 {
			logger.Infof("closing instance port ranges %v for %q",
				toClose, machined.tag)
			if err := instances[0].ClosePorts(machineId, toClose); err != nil {
				// TODO(mue) Add local retry logic.
				return err
			}
			network.SortPortRanges(toClose)
		}
	}
	return nil
}

// unitsChanged responds to changes to the assigned units.
func (fw *Firewaller) unitsChanged(change *unitsChange) error {
	changed := []*unitData{}
	for _, name := range change.units {
		unitTag := names.NewUnitTag(name)
		unit, err := fw.st.Unit(unitTag)
		if err != nil && !params.IsCodeNotFound(err) {
			return err
		}
		var machineTag names.MachineTag
		if unit != nil {
			machineTag, err = unit.AssignedMachine()
			if params.IsCodeNotFound(err) {
				continue
			} else if err != nil && !params.IsCodeNotAssigned(err) {
				return err
			}
		}
		if unitd, known := fw.unitds[unitTag]; known {
			knownMachineTag := fw.unitds[unitTag].machined.tag
			if unit == nil || unit.Life() == params.Dead || machineTag != knownMachineTag {
				fw.forgetUnit(unitd)
				changed = append(changed, unitd)
				logger.Debugf("stopped watching unit %s", name)
			}
			// TODO(dfc) fw.machineds should be map[names.Tag]
		} else if unit != nil && unit.Life() != params.Dead && fw.machineds[machineTag] != nil {
			err = fw.startUnit(unit, machineTag)
			if err != nil {
				return err
			}
			changed = append(changed, fw.unitds[unitTag])
			logger.Debugf("started watching %q", unitTag)
		}
	}
	if err := fw.flushUnits(changed); err != nil {
		return errors.Annotate(err, "cannot change firewall ports")
	}
	return nil
}

// openedPortsChanged handles port change notifications
func (fw *Firewaller) openedPortsChanged(machineTag names.MachineTag, networkTag names.NetworkTag) error {

	machined, ok := fw.machineds[machineTag]
	if !ok {
		// It is common to receive a port change notification before
		// registering the machine, so if a machine is not found in
		// firewaller's list, just skip the change.
		logger.Errorf("failed to lookup %q, skipping port change", machineTag)
		return nil
	}

	m, err := machined.machine()
	if err != nil {
		return err
	}

	ports, err := m.OpenedPorts(networkTag)
	if err != nil {
		return err
	}

	newPortRanges := make(map[network.PortRange]names.UnitTag)
	for portRange, unitTag := range ports {
		unitd, ok := machined.unitds[unitTag]
		if !ok {
			// It is common to receive port change notification before
			// registering a unit. Skip handling the port change - it will
			// be handled when the unit is registered.
			logger.Errorf("failed to lookup %q, skipping port change", unitTag)
			return nil
		}
		newPortRanges[portRange] = unitd.tag
	}

	if !portMapsEqual(machined.definedPorts, newPortRanges) {
		machined.definedPorts = newPortRanges
		return fw.flushMachine(machined)
	}
	return nil
}

func portMapsEqual(a, b map[network.PortRange]names.UnitTag) bool {
	if len(a) != len(b) {
		return false
	}
	for key, valueA := range a {
		valueB, exists := b[key]
		if !exists {
			return false
		}
		if valueA != valueB {
			return false
		}
	}
	return true
}

// flushUnits opens and closes ports for the passed unit data.
func (fw *Firewaller) flushUnits(unitds []*unitData) error {
	machineds := map[names.MachineTag]*machineData{}
	for _, unitd := range unitds {
		machineds[unitd.machined.tag] = unitd.machined
	}
	for _, machined := range machineds {
		if err := fw.flushMachine(machined); err != nil {
			return err
		}
	}
	return nil
}

// flushMachine opens and closes ports for the passed machine.
func (fw *Firewaller) flushMachine(machined *machineData) error {
	// Gather ports to open and close.
	want := []network.PortRange{}
	for portRange, unitTag := range machined.definedPorts {
		unitd, known := machined.unitds[unitTag]
		if !known {
			delete(machined.unitds, unitTag)
			continue
		}
		if unitd.serviced.exposed {
			want = append(want, portRange)
		}
	}
	toOpen := diffRanges(want, machined.openedPorts)
	toClose := diffRanges(machined.openedPorts, want)
	machined.openedPorts = want
	if fw.globalMode {
		return fw.flushGlobalPorts(toOpen, toClose)
	}
	return fw.flushInstancePorts(machined, toOpen, toClose)
}

// flushGlobalPorts opens and closes global ports in the environment.
// It keeps a reference count for ports so that only 0-to-1 and 1-to-0 events
// modify the environment.
func (fw *Firewaller) flushGlobalPorts(rawOpen, rawClose []network.PortRange) error {
	// Filter which ports are really to open or close.
	var toOpen, toClose []network.PortRange
	for _, portRange := range rawOpen {
		if fw.globalPortRef[portRange] == 0 {
			toOpen = append(toOpen, portRange)
		}
		fw.globalPortRef[portRange]++
	}
	for _, portRange := range rawClose {
		fw.globalPortRef[portRange]--
		if fw.globalPortRef[portRange] == 0 {
			toClose = append(toClose, portRange)
			delete(fw.globalPortRef, portRange)
		}
	}
	// Open and close the ports.
	if len(toOpen) > 0 {
		if err := fw.environ.OpenPorts(toOpen); err != nil {
			// TODO(mue) Add local retry logic.
			return err
		}
		network.SortPortRanges(toOpen)
		logger.Infof("opened port ranges %v in environment", toOpen)
	}
	if len(toClose) > 0 {
		if err := fw.environ.ClosePorts(toClose); err != nil {
			// TODO(mue) Add local retry logic.
			return err
		}
		network.SortPortRanges(toClose)
		logger.Infof("closed port ranges %v in environment", toClose)
	}
	return nil
}

// flushInstancePorts opens and closes ports global on the machine.
func (fw *Firewaller) flushInstancePorts(machined *machineData, toOpen, toClose []network.PortRange) error {
	// If there's nothing to do, do nothing.
	// This is important because when a machine is first created,
	// it will have no instance id but also no open ports -
	// InstanceId will fail but we don't care.
	if len(toOpen) == 0 && len(toClose) == 0 {
		return nil
	}
	m, err := machined.machine()
	if params.IsCodeNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	machineId := machined.tag.Id()
	instanceId, err := m.InstanceId()
	if err != nil {
		return err
	}
	instances, err := fw.environ.Instances([]instance.Id{instanceId})
	if err != nil {
		return err
	}
	// Open and close the ports.
	if len(toOpen) > 0 {
		if err := instances[0].OpenPorts(machineId, toOpen); err != nil {
			// TODO(mue) Add local retry logic.
			return err
		}
		network.SortPortRanges(toOpen)
		logger.Infof("opened port ranges %v on %q", toOpen, machined.tag)
	}
	if len(toClose) > 0 {
		if err := instances[0].ClosePorts(machineId, toClose); err != nil {
			// TODO(mue) Add local retry logic.
			return err
		}
		network.SortPortRanges(toClose)
		logger.Infof("closed port ranges %v on %q", toClose, machined.tag)
	}
	return nil
}

// machineLifeChanged starts watching new machines when the firewaller
// is starting, or when new machines come to life, and stops watching
// machines that are dying.
func (fw *Firewaller) machineLifeChanged(tag names.MachineTag) error {
	m, err := fw.st.Machine(tag)
	found := !params.IsCodeNotFound(err)
	if found && err != nil {
		return err
	}
	dead := !found || m.Life() == params.Dead
	machined, known := fw.machineds[tag]
	if known && dead {
		return fw.forgetMachine(machined)
	}
	if !known && !dead {
		err = fw.startMachine(tag)
		if err != nil {
			return err
		}
		logger.Debugf("started watching %q", tag)
	}
	return nil
}

// forgetMachine cleans the machine data after the machine is removed.
func (fw *Firewaller) forgetMachine(machined *machineData) error {
	for _, unitd := range machined.unitds {
		fw.forgetUnit(unitd)
	}
	if err := fw.flushMachine(machined); err != nil {
		return errors.Trace(err)
	}

	// Unusually, it's fine to ignore this error, because we know the machined
	// is being tracked in fw.catacomb. But we do still want to wait until the
	// watch loop has stopped before we nuke the last data and return.
	worker.Stop(machined)
	delete(fw.machineds, machined.tag)
	logger.Debugf("stopped watching %q", machined.tag)
	return nil
}

// forgetUnit cleans the unit data after the unit is removed.
func (fw *Firewaller) forgetUnit(unitd *unitData) {
	serviced := unitd.serviced
	machined := unitd.machined

	// If it's the last unit in the service, we'll need to stop the serviced.
	stoppedService := false
	if len(serviced.unitds) == 1 {
		if _, found := serviced.unitds[unitd.tag]; found {
			// Unusually, it's fine to ignore this error, because we know the
			// serviced is being tracked in fw.catacomb. But we do still want
			// to wait until the watch loop has stopped before we nuke the last
			// data and return.
			worker.Stop(serviced)
			stoppedService = true
		}
	}

	// Clean up after stopping.
	delete(fw.unitds, unitd.tag)
	delete(machined.unitds, unitd.tag)
	delete(serviced.unitds, unitd.tag)
	logger.Debugf("stopped watching %q", unitd.tag)
	if stoppedService {
		serviceTag := serviced.service.Tag()
		delete(fw.serviceds, serviceTag)
		logger.Debugf("stopped watching %q", serviceTag)
	}
}

// Kill is part of the worker.Worker interface.
func (fw *Firewaller) Kill() {
	fw.catacomb.Kill(nil)
}

// Wait is part of the worker.Worker interface.
func (fw *Firewaller) Wait() error {
	return fw.catacomb.Wait()
}

// unitsChange contains the changed units for one specific machine.
type unitsChange struct {
	machined *machineData
	units    []string
}

// machineData holds machine details and watches units added or removed.
type machineData struct {
	catacomb    catacomb.Catacomb
	fw          *Firewaller
	tag         names.MachineTag
	unitds      map[names.UnitTag]*unitData
	openedPorts []network.PortRange
	// ports defined by units on this machine
	definedPorts map[network.PortRange]names.UnitTag
}

func (md *machineData) machine() (*firewaller.Machine, error) {
	return md.fw.st.Machine(md.tag)
}

// watchLoop watches the machine for units added or removed.
func (md *machineData) watchLoop(unitw watcher.StringsWatcher) error {
	if err := md.catacomb.Add(unitw); err != nil {
		return errors.Trace(err)
	}
	for {
		select {
		case <-md.catacomb.Dying():
			return md.catacomb.ErrDying()
		case change, ok := <-unitw.Changes():
			if !ok {
				return errors.New("machine units watcher closed")
			}
			select {
			case md.fw.unitsChange <- &unitsChange{md, change}:
			case <-md.catacomb.Dying():
				return md.catacomb.ErrDying()
			}
		}
	}
}

// Kill is part of the worker.Worker interface.
func (md *machineData) Kill() {
	md.catacomb.Kill(nil)
}

// Wait is part of the worker.Worker interface.
func (md *machineData) Wait() error {
	return md.catacomb.Wait()
}

// unitData holds unit details.
type unitData struct {
	fw       *Firewaller
	tag      names.UnitTag
	unit     *firewaller.Unit
	serviced *serviceData
	machined *machineData
}

// exposedChange contains the changed exposed flag for one specific service.
type exposedChange struct {
	serviced *serviceData
	exposed  bool
}

// serviceData holds service details and watches exposure changes.
type serviceData struct {
	catacomb catacomb.Catacomb
	fw       *Firewaller
	service  *firewaller.Service
	exposed  bool
	unitds   map[names.UnitTag]*unitData
}

// watchLoop watches the service's exposed flag for changes.
func (sd *serviceData) watchLoop(exposed bool) error {
	serviceWatcher, err := sd.service.Watch()
	if err != nil {
		return errors.Trace(err)
	}
	if err := sd.catacomb.Add(serviceWatcher); err != nil {
		return errors.Trace(err)
	}
	for {
		select {
		case <-sd.catacomb.Dying():
			return sd.catacomb.ErrDying()
		case _, ok := <-serviceWatcher.Changes():
			if !ok {
				return errors.New("service watcher closed")
			}
			if err := sd.service.Refresh(); err != nil {
				if !params.IsCodeNotFound(err) {
					return errors.Trace(err)
				}
				return nil
			}
			change, err := sd.service.IsExposed()
			if err != nil {
				return errors.Trace(err)
			}
			if change == exposed {
				continue
			}

			exposed = change
			select {
			case sd.fw.exposedChange <- &exposedChange{sd, change}:
			case <-sd.catacomb.Dying():
				return sd.catacomb.ErrDying()
			}
		}
	}
}

// Kill is part of the worker.Worker interface.
func (sd *serviceData) Kill() {
	sd.catacomb.Kill(nil)
}

// Wait is part of the worker.Worker interface.
func (sd *serviceData) Wait() error {
	return sd.catacomb.Wait()
}

// diffRanges returns all the port rangess that exist in A but not B.
func diffRanges(A, B []network.PortRange) (missing []network.PortRange) {
next:
	for _, a := range A {
		for _, b := range B {
			if a == b {
				continue next
			}
		}
		missing = append(missing, a)
	}
	return
}

// parsePortsKey parses a ports document global key coming from the
// ports watcher (e.g. "42:juju-public") and returns the machine and
// network tags from its components (in the last example "machine-42"
// and "network-juju-public").
func parsePortsKey(change string) (machineTag names.MachineTag, networkTag names.NetworkTag, err error) {
	defer errors.DeferredAnnotatef(&err, "invalid ports change %q", change)

	parts := strings.SplitN(change, ":", 2)
	if len(parts) != 2 {
		return names.MachineTag{}, names.NetworkTag{}, errors.Errorf("unexpected format")
	}
	machineId, networkName := parts[0], parts[1]
	return names.NewMachineTag(machineId), names.NewNetworkTag(networkName), nil
}
