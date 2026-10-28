package ec2

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// MinMaxSize is the smallest Max of a group builder when one is set: Desired
// is 1, and the certificate rotation puts a replacement in service before it
// retires the old one, so Max must be above Desired.
const MinMaxSize = 2

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
		// Mode is ModeGroup (also the empty value) or ModeOnDemand.
		Mode Mode
		// Proxy is the wake proxy; only with ModeOnDemand.
		Proxy *FleetProxy
		// LegacyGroup keeps group builders beside on-demand ones (ModeOnDemand
		// only). Nil means none; a non-nil empty map is refused.
		LegacyGroup map[string]FleetBuilder
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
		// MaxSize is the group's Max; 0 keeps the library's default.
		// ModeGroup only.
		MaxSize int
		// Zone is the availability zone of the on-demand instance and its
		// cache volume. Required with ModeOnDemand, refused otherwise.
		Zone string
		// DataVolumeGiB is the on-demand cache volume; 0 keeps the default.
		// ModeOnDemand only.
		DataVolumeGiB int
	}
)

// Archs are the architectures of the builders, sorted.
func (f Fleet) Archs() []string { return archs(f.Builders) }

// LegacyArchs are the architectures of the legacy group builders, sorted.
func (f Fleet) LegacyArchs() []string { return archs(f.LegacyGroup) }

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

	onDemand := false

	switch f.Mode {
	case "", ModeGroup:
		if f.Proxy != nil {
			add("proxy: only with mode %q", ModeOnDemand)
		}

		if f.LegacyGroup != nil {
			add("legacy_group: only with mode %q (in group mode the builders are the groups)", ModeOnDemand)
		}
	case ModeOnDemand:
		onDemand = true
	default:
		add("mode %q: want %q (default) or %q", f.Mode, ModeGroup, ModeOnDemand)
	}

	if f.Proxy != nil && f.Proxy.InstanceType != "" && !typeRe.MatchString(f.Proxy.InstanceType) {
		add("proxy.instance_type %q: want an instance type", f.Proxy.InstanceType)
	}

	if f.LegacyGroup != nil {
		for _, arch := range f.LegacyArchs() {
			validateFleetBuilder("legacy_group.builders", arch, f.LegacyGroup[arch], false, add)
		}

		if len(f.LegacyGroup) == 0 {
			add("legacy_group.builders is empty")
		}

		for arch, b := range f.LegacyGroup {
			if o, ok := f.Builders[arch]; ok && o.Listener == b.Listener {
				add("legacy_group.builders.%s and builders.%s share listener %q", arch, arch, b.Listener)
			}
		}
	}

	if len(f.Builders) == 0 {
		add("builders is empty")
	}

	for _, arch := range f.Archs() {
		validateFleetBuilder("builders", arch, f.Builders[arch], onDemand, add)
	}

	return errors.Join(errs...)
}

func validateFleetBuilder(where, arch string, b FleetBuilder, onDemand bool, add func(string, ...any)) {
	where += "." + arch

	if _, ok := imageParameters[arch]; !ok {
		add("%s: want amd64 or arm64", where)
	}

	if b.Listener == "" || !typeRe.MatchString(b.InstanceType) || b.MaxParallelism < 1 {
		add("%s: want a listener, an instance type and max_parallelism >= 1", where)
	}

	switch {
	case onDemand && !zoneRe.MatchString(b.Zone):
		add("%s.zone %q: want an availability zone such as eu-central-1a (mode %q)", where, b.Zone, ModeOnDemand)
	case onDemand && (b.MaxSize != 0 || b.DataVolumeGiB < 0):
		add("%s: max_size is the group's (there is no group in mode %q); data_volume_gib must not be negative", where, ModeOnDemand)
	case !onDemand && (b.Zone != "" || b.DataVolumeGiB != 0):
		add("%s: zone and data_volume_gib are for mode %q only", where, ModeOnDemand)
	}

	if !onDemand && b.MaxSize != 0 && b.MaxSize < MinMaxSize {
		add("%s.max_size %d: want 0 (the library's default) or >= %d: one instance is in service, and the certificate rotation "+
			"puts a replacement in service before it retires the old one, so Max must be above Desired", where, b.MaxSize, MinMaxSize)
	}
}
