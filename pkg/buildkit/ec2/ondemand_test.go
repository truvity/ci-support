package ec2_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg/front"

	"github.com/truvity/ci-support/pkg/buildkit/ec2"
)

// The digest is a placeholder: the tests never download.
var testSHA = strings.Repeat("ab", 32)

func onDemandArgs() *ec2.Args { return args() }

func proxyArgs() *ec2.ProxyArgs {
	return &ec2.ProxyArgs{
		Name:      "devel-buildkit-proxy",
		Region:    testRegion,
		VPCID:     pulumi.String("vpc-0example"),
		SubnetIDs: []pulumi.StringInput{pulumi.String("subnet-0a"), pulumi.String("subnet-0b")},
		NLB:       nlb(),
		Sources:   sources(),
		Routes: []ec2.ProxyRoute{
			{Name: "arm64", Port: 1235, TargetTagValue: "devel-buildkit-arm64"},
			{Name: "amd64", Port: 1236, TargetTagValue: "devel-buildkit-amd64"},
		},
		SHA256:     testSHA,
		KMSKeyARNs: []string{"arn:" + part + ":kms:" + testRegion + ":" + testAccount + ":key/0example-key"},
	}
}

func runProxy(t *testing.T, a *ec2.ProxyArgs) (*recorder, *ec2.Proxy, error) {
	t.Helper()

	rec := &recorder{}

	var p *ec2.Proxy

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		var err error
		p, err = ec2.NewProxy(ctx, "buildkit-proxy", a)

		return err
	}, pulumi.WithMocks("p", "s", rec))

	return rec, p, err
}

func policyOf(t *testing.T, rec *recorder, role string) string {
	t.Helper()

	return rec.find(t, "aws:iam/role:Role", role)["inlinePolicies"].([]any)[0].(map[string]any)["policy"].(string)
}

func TestOnDemandResourceSet(t *testing.T) {
	rec, b, err := run(t, onDemandArgs())
	if err != nil {
		t.Fatal(err)
	}

	golden(t, "ondemand.resources.golden.json", rec.dump(t))

	if b.Box.Group != nil || b.Box.Instance == nil || b.Box.DataVolume == nil {
		t.Errorf("want one instance with a data volume and no group: %+v", b)
	}

	for _, x := range rec.all {
		switch typ := x["type"].(string); typ {
		case "aws:autoscaling/group:Group", "aws:autoscaling/lifecycleHook:LifecycleHook", "aws:lb/targetGroup:TargetGroup", "aws:lb/listener:Listener":
			t.Errorf("an on-demand builder created a %s", typ)
		}
	}

	// The cache outlives the instance: retained and protected is asg's, the
	// encryption and the key are ours.
	vol := rec.find(t, "aws:ebs/volume:Volume", "buildkit-arm64-data")
	if vol["encrypted"] != true || vol["size"] != float64(100) || vol["type"] != "gp3" || vol["availabilityZone"] != "eu-west-1a" {
		t.Errorf("volume %v", vol)
	}

	inst := rec.find(t, "aws:ec2/instance:Instance", "buildkit-arm64")
	if inst["tags"].(map[string]any)["role"] != "devel-buildkit-arm64" || b.RoleTagValue != "devel-buildkit-arm64" {
		t.Errorf("role tag %v %q", inst["tags"], b.RoleTagValue)
	}
}

func TestOnDemandUserDataGolden(t *testing.T) {
	_, b, err := run(t, onDemandArgs())
	if err != nil {
		t.Fatal(err)
	}

	if b.Rendered.Size > 16384 || len(b.Rendered.Objects) != 0 {
		t.Errorf("user-data is %d bytes in %d objects", b.Rendered.Size, len(b.Rendered.Objects))
	}

	golden(t, "ondemand.userdata.golden.yaml", b.Rendered.CloudConfig)

	ud := b.Rendered.CloudConfig

	for _, want := range []string{
		// the daemon starts on every boot and needs the certificate unit and the volume
		"Requires=buildkit-imds-guard.service buildkit-cert.service", "WantedBy=multi-user.target",
		"ExecStartPre=+/usr/bin/chown buildkit:buildkit /var/lib/buildkit",
		// the volume, mounted by asg's module, ordered before the daemon
		"asg-data-volume.service", "MOUNT_POINT='/var/lib/buildkit'", "LABEL='bkcache'",
		// the certificate: fetched on every boot, checked in place
		"ExecStart=/usr/local/lib/asg/ssmcert-buildkit.sh load", "STANDBY=0", "OWNER='buildkit:buildkit'",
		"RESTART_UNITS=('buildkitd.service')", "buildkit-cert-check.timer", "Unit=asg-ssmcert-buildkit.service",
		"WINDOW_START=0", "WINDOW_END=23", "DRAIN_PORT=1234",
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("user-data lacks %s", want)
		}
	}

	for _, refuse := range []string{
		// no lifecycle: no in-service runner, no ready gate, no watchdog, no standby rotation, no group named
		// (the rotation script carries the Standby code, which STANDBY=0 never reaches)
		"asg-in-service", "bootstrap-ready", "asg-watchdog.timer", "STANDBY=1", "GROUP='devel", "HOOK='devel",
		// the cache volume of the group mode is the datavolume module's now
		"/etc/fstab", "mkfs.xfs -q -L",
		// no unit is left for a boot that never comes: the daemon is enabled, not started by a step
		"systemctl start buildkitd.service",
	} {
		if strings.Contains(ud, refuse) {
			t.Errorf("user-data holds %s", refuse)
		}
	}
}

func TestOnDemandPolicy(t *testing.T) {
	rec, _, err := run(t, onDemandArgs())
	if err != nil {
		t.Fatal(err)
	}

	pol := policyOf(t, rec, "buildkit-arm64")
	golden(t, "ondemand.policy.golden.json", pol+"\n")

	for _, want := range []string{`"kms:Decrypt"`, `"alias/test-ci-buildkit-server"`, "parameter/ci/test/buildkit-server/arm64/key"} {
		if !strings.Contains(pol, want) {
			t.Errorf("policy lacks %s", want)
		}
	}

	// No group, no lifecycle, no standby: nothing of autoscaling in the role.
	for _, refuse := range []string{"autoscaling:", "ecr:", "s3:", `"kms:Encrypt"`} {
		if strings.Contains(pol, refuse) {
			t.Errorf("policy holds %s", refuse)
		}
	}
}

func TestOnDemandOnlyTheProxyReachesTheBuilder(t *testing.T) {
	rec, _, err := run(t, onDemandArgs())
	if err != nil {
		t.Fatal(err)
	}

	var rules []map[string]any

	for _, x := range rec.all {
		if x["type"] == "aws:vpc/securityGroupIngressRule:SecurityGroupIngressRule" {
			rules = append(rules, x["inputs"].(map[string]any))
		}
	}

	if len(rules) != 1 || rules[0]["referencedSecurityGroupId"] != "sg-0proxy" || rules[0]["fromPort"] != float64(1234) || rules[0]["toPort"] != float64(1234) {
		t.Fatalf("rules %v", rules)
	}
}

func TestOnDemandRefusals(t *testing.T) {
	for name, mut := range map[string]func(*ec2.Args){
		"mode":       func(a *ec2.Args) { a.Mode = "spot" },
		"no options": func(a *ec2.Args) { a.OnDemand = nil },
		"subnet":     func(a *ec2.Args) { a.OnDemand.SubnetID = nil },
		"zone":       func(a *ec2.Args) { a.OnDemand.Zone = "eu-west-1" },
		"proxy sg":   func(a *ec2.Args) { a.OnDemand.ProxySecurityGroupID = nil },
		"data size":  func(a *ec2.Args) { a.OnDemand.DataVolumeGiB = 10 },
		"key":        func(a *ec2.Args) { a.OnDemand.DataKMSKeyARN = "alias/x" },
		"tag key":    func(a *ec2.Args) { a.OnDemand.RoleTagKey = "aws:role" },
		"port":       func(a *ec2.Args) { a.Port = 80 },
		"cert":       func(a *ec2.Args) { a.Certificate.Key = "" },
		"rotation":   func(a *ec2.Args) { a.Rotation.Name = "g" },
		"daemon":     func(a *ec2.Args) { a.Daemon.MaxParallelism = 0 },
	} {
		a := onDemandArgs()
		mut(a)

		if _, _, err := run(t, a); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestProxyResourceSet(t *testing.T) {
	rec, p, err := runProxy(t, proxyArgs())
	if err != nil {
		t.Fatal(err)
	}

	golden(t, "proxy.resources.golden.json", rec.dump(t))

	if p.GroupName != "devel-buildkit-proxy" || p.HealthPort != 8080 {
		t.Errorf("outputs %q %d", p.GroupName, p.HealthPort)
	}

	g := rec.find(t, "aws:autoscaling/group:Group", "buildkit-proxy")
	if g["desiredCapacity"] != float64(1) || g["healthCheckType"] != "ELB" || len(g["targetGroupArns"].([]any)) != 2 {
		t.Errorf("group %v", g)
	}

	// Desired 1 and one stopped spare in the pool, and the launch hook.
	pool, _ := g["warmPool"].(map[string]any)
	if pool["minSize"] != float64(1) || pool["poolState"] != "Stopped" {
		t.Errorf("warm pool %v", pool)
	}

	if g["initialLifecycleHooks"] == nil {
		t.Errorf("the proxy has no launch hook: %v", g)
	}

	// One listener per route, on the builder's registered port, with an idle
	// timeout above the wake timeout, and an HTTP health check on the
	// proxy's health port.
	var listeners, targets int

	for _, x := range rec.all {
		in := x["inputs"].(map[string]any)

		switch x["type"] {
		case "aws:lb/listener:Listener":
			listeners++

			if in["tcpIdleTimeoutSeconds"] != float64(3600) || in["protocol"] != "TCP" {
				t.Errorf("listener %v", in)
			}
		case "aws:lb/targetGroup:TargetGroup":
			targets++

			hc := in["healthCheck"].(map[string]any)
			if hc["protocol"] != "HTTP" || hc["path"] != "/healthz" || hc["port"] != "8080" {
				t.Errorf("health check %v", hc)
			}
		}
	}

	if listeners != 2 || targets != 2 {
		t.Errorf("%d listeners, %d target groups", listeners, targets)
	}
}

// metricsProxyArgs publishes the proxy's metrics on the shared NLB.
func metricsProxyArgs() *ec2.ProxyArgs {
	a := proxyArgs()
	a.MetricsListenPort = 9102
	a.MetricsSources = []front.Source{{CIDR: "10.0.0.0/16", Description: "metrics scraper"}}

	return a
}

func TestProxyMetricsListener(t *testing.T) {
	rec, p, err := runProxy(t, metricsProxyArgs())
	if err != nil {
		t.Fatal(err)
	}

	golden(t, "proxy-metrics.resources.golden.json", rec.dump(t))
	golden(t, "proxy-metrics.userdata.golden.yaml", p.Rendered.CloudConfig)

	if p.MetricsAddress != "0.0.0.0:9102" || p.MetricsListenPort != 9102 {
		t.Errorf("outputs %q %d", p.MetricsAddress, p.MetricsListenPort)
	}

	if !strings.Contains(p.Rendered.CloudConfig, "0.0.0.0:9102") {
		t.Error("the proxy does not bind the metrics on all interfaces")
	}

	g := rec.find(t, "aws:autoscaling/group:Group", "buildkit-proxy")
	if len(g["targetGroupArns"].([]any)) != 3 {
		t.Errorf("the group is attached to %v", g["targetGroupArns"])
	}

	var listeners, targets, rule int

	for _, x := range rec.all {
		in := x["inputs"].(map[string]any)

		switch x["type"] {
		case "aws:lb/listener:Listener":
			listeners++
		case "aws:lb/targetGroup:TargetGroup":
			targets++
		case "aws:vpc/securityGroupIngressRule:SecurityGroupIngressRule":
			if in["fromPort"] == float64(9102) && in["toPort"] == float64(9102) && in["referencedSecurityGroupId"] != nil {
				rule++
			}
		}
	}

	if listeners != 3 || targets != 3 || rule != 1 {
		t.Errorf("%d listeners, %d target groups, %d proxy ingress rules on 9102", listeners, targets, rule)
	}
}

func TestProxyMetricsAddressKept(t *testing.T) {
	a := metricsProxyArgs()
	a.MetricsAddress = "10.1.2.3:9102"

	if _, p, err := runProxy(t, a); err != nil || p.MetricsAddress != "10.1.2.3:9102" {
		t.Errorf("%v", err)
	}
}

func TestProxyMetricsRefusals(t *testing.T) {
	for name, mut := range map[string]func(*ec2.ProxyArgs){
		"no sources":        func(a *ec2.ProxyArgs) { a.MetricsSources = nil },
		"route port":        func(a *ec2.ProxyArgs) { a.MetricsListenPort = a.Routes[0].Port },
		"health port":       func(a *ec2.ProxyArgs) { a.MetricsListenPort = a.HealthPort + 0; a.HealthPort = 9102 },
		"low port":          func(a *ec2.ProxyArgs) { a.MetricsListenPort = 80 },
		"loopback":          func(a *ec2.ProxyArgs) { a.MetricsAddress = "127.0.0.1:9102" },
		"other port":        func(a *ec2.ProxyArgs) { a.MetricsAddress = "0.0.0.0:9103" },
		"listener clash":    func(a *ec2.ProxyArgs) { a.MetricsListener = "buildkit-arm64" },
		"sources w/o port":  func(a *ec2.ProxyArgs) { a.MetricsListenPort = 0; a.MetricsAddress = "" },
		"world not allowed": func(a *ec2.ProxyArgs) { a.MetricsSources = []front.Source{{CIDR: "0.0.0.0/0"}} },
	} {
		a := metricsProxyArgs()
		mut(a)

		if _, _, err := runProxy(t, a); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestProxyUserDataGolden(t *testing.T) {
	_, p, err := runProxy(t, proxyArgs())
	if err != nil {
		t.Fatal(err)
	}

	golden(t, "proxy.userdata.golden.yaml", p.Rendered.CloudConfig)

	ud := p.Rendered.CloudConfig

	for _, want := range []string{
		"releases/download/v0.9.1/wake-proxy_0.9.1_linux_arm64", testSHA, `"listenPort": 1235`, `"listenPort": 1236`, `"targetPort": 1234`,
		`"tagValue": "devel-buildkit-arm64"`, `"tagValue": "devel-buildkit-amd64"`, `"wakeTimeout": "3m0s"`, `"idleTimeout": "20m0s"`,
	} {
		if !strings.Contains(strings.ReplaceAll(ud, "<sha256>", testSHA), want) {
			t.Errorf("user-data lacks %s", want)
		}
	}
}

func TestProxyPolicy(t *testing.T) {
	rec, _, err := runProxy(t, proxyArgs())
	if err != nil {
		t.Fatal(err)
	}

	pol := policyOf(t, rec, "buildkit-proxy")
	golden(t, "proxy.policy.golden.json", pol+"\n")

	var doc struct {
		Statement []struct {
			Sid       string
			Action    []string
			Resource  []string
			Condition map[string]map[string][]string
		}
	}

	if err := json.Unmarshal([]byte(pol), &doc); err != nil {
		t.Fatal(err)
	}

	sids := map[string]bool{}
	for _, s := range doc.Statement {
		sids[s.Sid] = true
	}

	for _, want := range []string{
		"DescribeInstances", "StartStopTargets1", "StartStopTargets2", "TagActivity1", "TagActivity2", "UseTargetVolumeKeys", "GrantTargetVolumeKeys",
	} {
		if !sids[want] {
			t.Errorf("policy lacks statement %s", want)
		}
	}

	// Start and stop only where the role tag names one of the builders, the
	// key only for the volumes' key.
	for _, s := range doc.Statement {
		switch s.Sid {
		case "StartStopTargets1", "StartStopTargets2":
			if len(s.Condition["StringEquals"]) != 1 {
				t.Errorf("%s: condition %v", s.Sid, s.Condition)
			}
		case "UseTargetVolumeKeys", "GrantTargetVolumeKeys":
			if len(s.Resource) != 1 || !strings.Contains(s.Resource[0], ":key/0example-key") {
				t.Errorf("%s: resource %v", s.Sid, s.Resource)
			}
		}
	}

	// The builder's tag in the proxy's condition is the tag the builder carries.
	for _, v := range []string{"devel-buildkit-arm64", "devel-buildkit-amd64"} {
		if !strings.Contains(pol, `"`+v+`"`) {
			t.Errorf("policy does not name %s", v)
		}
	}
}

// The pinned digests are the ones the release published, and the default is
// used when the caller names none.
func TestProxyDefaultDigest(t *testing.T) {
	a := proxyArgs()
	a.SHA256 = ""

	_, p, err := runProxy(t, a)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(p.Rendered.CloudConfig, ec2.WakeProxySHA256["arm64"]) {
		t.Error("the user-data does not carry the pinned arm64 digest")
	}

	// Another version has no pinned digest: it must be named.
	a.Version = "v0.6.1"

	if _, _, err := runProxy(t, a); err == nil {
		t.Error("an unpinned version without a digest was accepted")
	}
}

func TestProxyRefusals(t *testing.T) {
	for name, mut := range map[string]func(*ec2.ProxyArgs){
		"routes":       func(a *ec2.ProxyArgs) { a.Routes = nil },
		"sha":          func(a *ec2.ProxyArgs) { a.SHA256 = ""; a.Version = "v0.6.1" },
		"sha shape":    func(a *ec2.ProxyArgs) { a.SHA256 = "abc" },
		"region":       func(a *ec2.ProxyArgs) { a.Region = "" },
		"nlb":          func(a *ec2.ProxyArgs) { a.NLB = front.SharedNLB{} },
		"sources":      func(a *ec2.ProxyArgs) { a.Sources = nil },
		"same port":    func(a *ec2.ProxyArgs) { a.Routes[1].Port = a.Routes[0].Port },
		"health port":  func(a *ec2.ProxyArgs) { a.HealthPort = a.Routes[1].Port },
		"same target":  func(a *ec2.ProxyArgs) { a.Routes[1].TargetTagValue = a.Routes[0].TargetTagValue },
		"no target":    func(a *ec2.ProxyArgs) { a.Routes[0].TargetTagValue = "" },
		"route name":   func(a *ec2.ProxyArgs) { a.Routes[0].Name = "ARM" },
		"bad key":      func(a *ec2.ProxyArgs) { a.KMSKeyARNs = []string{"alias/data"} },
		"wildcard key": func(a *ec2.ProxyArgs) { a.KMSKeyARNs = []string{"arn:" + part + ":kms:*:*:key/*"} },
		"version":      func(a *ec2.ProxyArgs) { a.Version = "latest" },
		// the NLB would cut the client's quiet connection before the box is up
		"wake over idle": func(a *ec2.ProxyArgs) { a.WakeTimeout = 2 * time.Hour },
		"arch":           func(a *ec2.ProxyArgs) { a.Arch = "x86" },
	} {
		a := proxyArgs()
		mut(a)

		if _, _, err := runProxy(t, a); err == nil {
			t.Errorf("%s: no error", name)
		}
	}

	if _, _, err := runProxy(t, nil); err == nil {
		t.Error("nil args: no error")
	}
}

// The routes of the proxy and the builders agree: the builder's tag value,
// the port the proxy listens on and the port the builder's group admits.
func TestProxyAndBuilderAgree(t *testing.T) {
	b := onDemandArgs()
	p := proxyArgs()

	_, bb, err := run(t, b)
	if err != nil {
		t.Fatal(err)
	}

	if bb.RoleTagKey != p.RoleTagKey && bb.RoleTagKey != "role" {
		t.Errorf("tag key %q", bb.RoleTagKey)
	}

	if bb.RoleTagValue != p.Routes[0].TargetTagValue || b.Port != p.Routes[0].Port {
		t.Errorf("the arm64 route does not match the arm64 builder: %q %d", bb.RoleTagValue, b.Port)
	}
}
