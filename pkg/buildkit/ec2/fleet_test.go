package ec2_test

import (
	"strings"
	"testing"

	"github.com/truvity/ci-support/pkg/buildkit/ec2"
)

type fb = ec2.FleetBuilder

func one(b ec2.FleetBuilder) map[string]ec2.FleetBuilder {
	return map[string]ec2.FleetBuilder{"arm64": b}
}

func sized(max int) ec2.FleetBuilder {
	return fb{Listener: "l", InstanceType: "m7g.xlarge", MaxParallelism: 1, MaxSize: max}
}

func withZone(b ec2.FleetBuilder) ec2.FleetBuilder { b.Zone = "eu-central-1a"; return b }

func TestFleetValidate(t *testing.T) {
	grp := ec2.FleetBuilder{Listener: "buildkit-arm64", InstanceType: "m7g.xlarge", MaxParallelism: 4}
	od := ec2.FleetBuilder{Listener: "buildkit-arm64", InstanceType: "m7g.xlarge", MaxParallelism: 4, Zone: "eu-central-1a"}

	cases := []struct {
		name string
		f    ec2.Fleet
		want string // substring of the error; empty = valid
	}{
		{"group", ec2.Fleet{Builders: one(grp)}, ""},
		{"group max 3", ec2.Fleet{Builders: one(sized(3))}, ""},
		{"group max 1", ec2.Fleet{Builders: one(sized(1))}, "max_size 1"},
		{"group with proxy", ec2.Fleet{Proxy: &ec2.FleetProxy{}, Builders: one(grp)}, "proxy: only"},
		{"group with zone", ec2.Fleet{Builders: one(od)}, "for mode"},
		{"on-demand", ec2.Fleet{
			Mode: ec2.ModeOnDemand, Proxy: &ec2.FleetProxy{InstanceType: "t4g.nano"}, Builders: one(od)}, ""},
		{"on-demand no zone", ec2.Fleet{Mode: ec2.ModeOnDemand, Builders: one(grp)}, "availability zone"},
		{"on-demand max_size", ec2.Fleet{Mode: ec2.ModeOnDemand, Builders: one(withZone(sized(3)))}, "no group"},
		{"bad mode", ec2.Fleet{Mode: "x", Builders: one(grp)}, `mode "x"`},
		{"bad proxy type", ec2.Fleet{Mode: ec2.ModeOnDemand, Proxy: &ec2.FleetProxy{InstanceType: "Nano"}, Builders: one(od)}, "proxy.instance_type"},
		{"bad arch", ec2.Fleet{Builders: map[string]ec2.FleetBuilder{"s390x": grp}}, "amd64 or arm64"},
		{"no builders", ec2.Fleet{}, "builders is empty"},
		{"bad parallelism", ec2.Fleet{Builders: one(fb{Listener: "l", InstanceType: "m7g.xlarge"})}, "max_parallelism"},
		{"legacy in group mode", ec2.Fleet{LegacyGroup: one(grp), Builders: one(grp)}, "legacy_group: only"},
		{"legacy shares listener", ec2.Fleet{Mode: ec2.ModeOnDemand, LegacyGroup: one(grp), Builders: one(od)}, "share listener"},
		{"legacy ok", ec2.Fleet{Mode: ec2.ModeOnDemand,
			LegacyGroup: one(fb{Listener: "old", InstanceType: "m7g.xlarge", MaxParallelism: 4}),
			Builders:    one(od)}, ""},
		{"legacy empty", ec2.Fleet{Mode: ec2.ModeOnDemand, LegacyGroup: map[string]ec2.FleetBuilder{},
			Builders: one(od)}, "legacy_group.builders is empty"},
	}

	for _, tc := range cases {
		err := tc.f.Validate()

		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}
