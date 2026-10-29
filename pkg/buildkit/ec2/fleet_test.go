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

func TestFleetValidate(t *testing.T) {
	od := ec2.FleetBuilder{Listener: "buildkit-arm64", InstanceType: "m7g.xlarge", MaxParallelism: 4, Zone: "eu-central-1a"}
	negVolume := od
	negVolume.DataVolumeGiB = -1
	noZone := ec2.FleetBuilder{Listener: "buildkit-arm64", InstanceType: "m7g.xlarge", MaxParallelism: 4}

	cases := []struct {
		name string
		f    ec2.Fleet
		want string // substring of the error; empty = valid
	}{
		{"default mode", ec2.Fleet{Builders: one(od)}, ""},
		{"on-demand", ec2.Fleet{
			Mode: ec2.ModeOnDemand, Proxy: &ec2.FleetProxy{InstanceType: "t4g.nano"}, Builders: one(od)}, ""},
		{"no zone", ec2.Fleet{Mode: ec2.ModeOnDemand, Builders: one(noZone)}, "availability zone"},
		{"negative volume", ec2.Fleet{Builders: one(negVolume)}, "data_volume_gib"},
		{"group mode is gone", ec2.Fleet{Mode: "group", Builders: one(od)}, `mode "group"`},
		{"bad proxy type", ec2.Fleet{Mode: ec2.ModeOnDemand, Proxy: &ec2.FleetProxy{InstanceType: "Nano"}, Builders: one(od)}, "proxy.instance_type"},
		{"bad arch", ec2.Fleet{Builders: map[string]ec2.FleetBuilder{"s390x": od}}, "amd64 or arm64"},
		{"no builders", ec2.Fleet{}, "builders is empty"},
		{"bad parallelism", ec2.Fleet{Builders: one(fb{Listener: "l", InstanceType: "m7g.xlarge", Zone: "eu-central-1a"})}, "max_parallelism"},
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
