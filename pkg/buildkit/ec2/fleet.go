package ec2

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
)

var (
	typeRe = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z0-9]+$`)
)

type (
	// Fleet is the choices a caller makes for all the builders of one
	// environment, before any resource exists: the shape of a configuration
	// file, free of provider inputs. [Fleet.Validate] holds it to the rules
	// the library itself applies when [New] and [NewProxy] run, so a file is
	// refused at load time and not at preview. Estate facts (listeners that
	// exist, security groups, parameter names) stay with the caller.
	Fleet struct {
		// Mode is ModeOnDemand, also the empty value.
		Mode Mode
		// Proxy is the wake proxy.
		Proxy *FleetProxy
		// Builders are the builders by architecture (amd64, arm64).
		Builders map[string]FleetBuilder
	}

	// FleetProxy is the wake proxy's choices.
	FleetProxy struct {
		// InstanceType is empty for the library's default.
		InstanceType string
	}

	// FleetBuilder is one architecture's choices.
	FleetBuilder struct {
		// Listener names the NLB listener.
		Listener     string
		InstanceType string
		// MaxParallelism is buildkitd's max-parallelism, at least 1.
		MaxParallelism int
		// Zone is the availability zone of the instance and its cache
		// volume. Required.
		Zone string
		// DataVolumeGiB is the cache volume; 0 keeps the default.
		DataVolumeGiB int
	}
)

// Archs are the architectures of the builders, sorted.
func (f Fleet) Archs() []string { return archs(f.Builders) }

func archs(m map[string]FleetBuilder) []string {
	out := make([]string, 0, len(m))
	for a := range m {
		out = append(out, a)
	}

	sort.Strings(out)

	return out
}

// Validate reports every problem at once.
func (f Fleet) Validate() error {
	var errs []error

	add := func(format string, v ...any) { errs = append(errs, fmt.Errorf(format, v...)) }

	switch f.Mode {
	case "", ModeOnDemand:
	default:
		add("mode %q: want %q (the only mode)", f.Mode, ModeOnDemand)
	}

	if f.Proxy != nil && f.Proxy.InstanceType != "" && !typeRe.MatchString(f.Proxy.InstanceType) {
		add("proxy.instance_type %q: want an instance type", f.Proxy.InstanceType)
	}

	if len(f.Builders) == 0 {
		add("builders is empty")
	}

	for _, arch := range f.Archs() {
		validateFleetBuilder("builders", arch, f.Builders[arch], add)
	}

	return errors.Join(errs...)
}

func validateFleetBuilder(where, arch string, b FleetBuilder, add func(string, ...any)) {
	where += "." + arch

	if _, ok := imageParameters[arch]; !ok {
		add("%s: want amd64 or arm64", where)
	}

	if b.Listener == "" || !typeRe.MatchString(b.InstanceType) || b.MaxParallelism < 1 {
		add("%s: want a listener, an instance type and max_parallelism >= 1", where)
	}

	if !zoneRe.MatchString(b.Zone) {
		add("%s.zone %q: want an availability zone such as eu-central-1a", where, b.Zone)
	}

	if b.DataVolumeGiB < 0 {
		add("%s.data_volume_gib must not be negative", where)
	}
}
