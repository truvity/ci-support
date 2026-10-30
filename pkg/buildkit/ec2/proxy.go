package ec2

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"time"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg"
	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/front"
	"github.com/truvity/asg/modules/wakeproxy"
	"github.com/truvity/asg/modules/watchdog"
)

// Defaults of the proxy.
const (
	// WakeProxyVersion is the truvity/asg release whose cmd/wake-proxy build
	// the proxy installs (the module version in go.mod moves with it).
	WakeProxyVersion = "v0.9.1"

	DefaultProxyInstanceType = "t4g.nano"
	DefaultProxyArch         = "arm64"
	DefaultProxyHealthPort   = wakeproxy.DefaultHealthPort
	// DefaultProxyMetricsListener names the metrics listener when
	// ProxyArgs.MetricsListenPort is set.
	DefaultProxyMetricsListener = "buildkit-proxy-metrics"
	// DefaultIdleTimeout is how long a builder may go without a connection
	// before the proxy stops it.
	DefaultIdleTimeout = 20 * time.Minute
	// DefaultWakeTimeout is how long the proxy holds a client while the
	// builder wakes. The listener's TCP idle timeout (DefaultIdleTimeoutSeconds)
	// must exceed it, since the client's connection is quiet meanwhile.
	DefaultWakeTimeout = 180 * time.Second
	// DefaultProxyHookHeartbeatSeconds bounds a proxy's boot into service.
	DefaultProxyHookHeartbeatSeconds = 600
)

// WakeProxySHA256 are the digests of the wake-proxy builds of
// WakeProxyVersion (the release's checksums.txt). A bump is by hand with the
// version, like the pinned BuildKit releases.
var WakeProxySHA256 = map[string]string{
	"amd64": "812cd128222ac43b2404ecb67e9067d6c7af26f85ac08dadce136681e4e6d334",
	"arm64": "3054f78e3380e3ac756a2b1ea84ad3a53df72463b8e8d3adb197bc39183744cb",
}

type (
	// ProxyRoute is one builder behind the proxy: the NLB listens on Port,
	// the proxy listens on the same Port on its instance, and pipes to the
	// builder's DaemonPort found by its role tag.
	ProxyRoute struct {
		// Name labels the route's logs and metrics, and the listener's name
		// when Listener is empty: lower-case letters, digits and -, such as
		// arm64. Required.
		Name string
		// Listener names the NLB listener and its target group. Default
		// buildkit-<Name>.
		Listener string
		// Port is the builder's registered listener port (Args.Port of the
		// builder). Required.
		Port int
		// TargetTagValue is the builder's role tag value
		// (Builder.RoleTagValue; default the builder's Name). Exactly one
		// instance may carry it.
		TargetTagValue string
	}

	// ProxyArgs configure the wake proxy of an environment's builders.
	ProxyArgs struct {
		// Name is the physical name of the proxy's group, launch template,
		// role and security group prefix (at most 48 characters), such as
		// devel-buildkit-proxy. Default: the logical name.
		Name string
		// InstanceType is a small instance of Arch. Default t4g.nano.
		InstanceType string
		// Arch is the proxy's CPU architecture: arm64 (default) or amd64.
		Arch string
		// Routes are the builders: one per architecture. Required.
		Routes []ProxyRoute
		// RoleTagKey is the key of the builders' role tag. Default role
		// (Args.OnDemand.RoleTagKey of every builder must be the same).
		RoleTagKey string

		// Region of the builders. Required: it is in the proxy's
		// configuration file, which the user-data carries.
		Region string
		// VPCID and SubnetIDs place the proxy (private subnets, several
		// zones). Required.
		VPCID     pulumi.StringInput
		SubnetIDs []pulumi.StringInput
		// RootVolumeGiB is the root volume. Default 8.
		RootVolumeGiB int
		// PermissionsBoundary is the role's boundary policy ARN.
		PermissionsBoundary string
		Tags                map[string]string
		Provider            *aws.Provider

		// NLB is the shared NLB (the same as the builders had). Required.
		NLB front.SharedNLB
		// Sources may connect to the routes' ports (the runners' security
		// group). At least one.
		Sources []front.Source

		// SHA256 is the digest of the wake-proxy release build for Arch, from
		// the release's checksums.txt; a build is installed only when it
		// matches. Default: the pinned digest of WakeProxyVersion
		// (WakeProxySHA256); required with another Version.
		SHA256 string
		// Version is the asg release. Default WakeProxyVersion.
		Version string
		// HealthPort serves GET /healthz for the NLB health check. Default
		// 8080.
		HealthPort int
		// MetricsAddress, when set (127.0.0.1:9102), serves Prometheus text
		// on the proxy itself: reachable only from the instance. To scrape it
		// across the VPC through the shared NLB, set MetricsListenPort.
		MetricsAddress string
		// MetricsListenPort, when set (1024..65535, from the NLB's port
		// registry), publishes the metrics on the shared NLB: a TCP listener
		// on this port, a target group forwarding to the same port on the
		// proxy instances (so it follows instance refreshes), the Auto
		// Scaling group attached to it, and a rule admitting the NLB's
		// security group to that port. Scrape http://<NLB DNS name>:<port>/metrics.
		// The proxy then binds 0.0.0.0:<port> unless MetricsAddress names a
		// non-loopback host with this same port. The metrics have no
		// authentication or TLS, so name only the scrapers in MetricsSources.
		// Zero keeps the metrics where MetricsAddress puts them and creates
		// nothing.
		MetricsListenPort int
		// MetricsListener names the metrics listener and its target group.
		// Default "buildkit-proxy-metrics".
		MetricsListener string
		// MetricsSources may connect to MetricsListenPort (the scraper's
		// security group or range). At least one when MetricsListenPort is
		// set; refused otherwise.
		MetricsSources []front.Source
		// IdleTimeout and WakeTimeout. Defaults 20m and 180s.
		IdleTimeout, WakeTimeout time.Duration

		// KMSKeyARNs are the customer-managed keys of the builders' data
		// volumes (OnDemand.DataKMSKeyARN). Without them, starting a builder
		// with an encrypted volume leaves it in pending.
		KMSKeyARNs []string
	}

	// Proxy is what NewProxy creates: the proxy's appliance and its listeners
	// on the shared NLB.
	Proxy struct {
		// Box is the proxy's Auto Scaling group and its parts.
		Box *asg.Appliance
		// Front holds the listeners and target groups, one per route.
		Front *front.Front
		// Rendered is the user-data, for review goldens.
		Rendered *bootstrap.Rendered
		// RoleTagKey is the key the routes select by.
		RoleTagKey string
		// HealthPort and MetricsAddress are what alerts scrape.
		// MetricsListenPort is the NLB port of the metrics (0 when they are
		// not published on the NLB).
		HealthPort        int
		MetricsAddress    string
		MetricsListenPort int
		// GroupName is the Auto Scaling group's name.
		GroupName string
	}
)

var (
	routeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	shaOnlyRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionRe   = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	// A placeholder 12-digit account for validating the statements before
	// the real account is known; built, not written, so no id is in source.
	probeAccount = func() string {
		b := make([]byte, 12)
		for i := range b {
			b[i] = '0'
		}

		return string(b)
	}()
)

func (a ProxyArgs) withDefaults(name string) ProxyArgs {
	if a.Name == "" {
		a.Name = name
	}

	if a.InstanceType == "" {
		a.InstanceType = DefaultProxyInstanceType
	}

	if a.Arch == "" {
		a.Arch = DefaultProxyArch
	}

	if a.RoleTagKey == "" {
		a.RoleTagKey = DefaultRoleTagKey
	}

	if a.RootVolumeGiB == 0 {
		a.RootVolumeGiB = DefaultRootVolumeGiB
	}

	if a.Version == "" {
		a.Version = WakeProxyVersion
	}

	if a.SHA256 == "" && a.Version == WakeProxyVersion {
		a.SHA256 = WakeProxySHA256[a.Arch]
	}

	if a.HealthPort == 0 {
		a.HealthPort = DefaultProxyHealthPort
	}

	if a.MetricsListenPort != 0 {
		if a.MetricsListener == "" {
			a.MetricsListener = DefaultProxyMetricsListener
		}

		if a.MetricsAddress == "" {
			a.MetricsAddress = "0.0.0.0:" + strconv.Itoa(a.MetricsListenPort)
		}
	}

	if a.IdleTimeout == 0 {
		a.IdleTimeout = DefaultIdleTimeout
	}

	if a.WakeTimeout == 0 {
		a.WakeTimeout = DefaultWakeTimeout
	}

	routes := make([]ProxyRoute, len(a.Routes))
	copy(routes, a.Routes)

	for i := range routes {
		if routes[i].Listener == "" {
			routes[i].Listener = "buildkit-" + routes[i].Name
		}
	}

	a.Routes = routes

	return a
}

func (a ProxyArgs) validate() error {
	var errs []error

	add := func(format string, v ...any) { errs = append(errs, fmt.Errorf(format, v...)) }

	if _, ok := imageParameters[a.Arch]; !ok {
		add("Arch %q: want amd64 or arm64", a.Arch)
	}

	if !nameRe.MatchString(a.Name) {
		add("Name %q: want lower-case letters, digits and -, at most 48", a.Name)
	}

	if a.VPCID == nil || len(a.SubnetIDs) == 0 {
		add("VPCID and SubnetIDs are required")
	}

	if a.Region == "" {
		add("Region is required")
	}

	if len(a.Sources) == 0 {
		add("Sources is empty: name who may connect (the runners' security group)")
	}

	if a.NLB == (front.SharedNLB{}) {
		add("NLB is required")
	}

	if !shaOnlyRe.MatchString(a.SHA256) {
		add("SHA256 %q: want the 64 hex digits of the wake-proxy build for %s from the release's checksums.txt", a.SHA256, a.Arch)
	}

	if !versionRe.MatchString(a.Version) {
		add("Version %q: want a release tag such as v0.6.0", a.Version)
	}

	if a.RootVolumeGiB < 8 {
		add("RootVolumeGiB %d: want at least 8", a.RootVolumeGiB)
	}

	if a.HealthPort < 1024 || a.HealthPort > 65535 {
		add("HealthPort %d: want 1024..65535", a.HealthPort)
	}

	if a.WakeTimeout < 10*time.Second || a.IdleTimeout < time.Minute {
		add("WakeTimeout %s and IdleTimeout %s: want at least 10s and 1m", a.WakeTimeout, a.IdleTimeout)
	}

	if a.WakeTimeout >= time.Duration(DefaultIdleTimeoutSeconds)*time.Second {
		add("WakeTimeout %s must be below the NLB listener's TCP idle timeout (%ds): the client's connection is quiet while the builder wakes",
			a.WakeTimeout, DefaultIdleTimeoutSeconds)
	}

	if len(a.Routes) == 0 {
		add("Routes is empty: one route per builder")
	}

	ports, names, listeners := map[int]bool{}, map[string]bool{}, map[string]bool{}
	targets := map[string]bool{}

	for i, r := range a.Routes {
		switch {
		case !routeNameRe.MatchString(r.Name):
			add("Routes[%d].Name %q: want lower-case letters, digits and -", i, r.Name)
		case names[r.Name]:
			add("Routes[%d].Name %q is used twice", i, r.Name)
		}

		names[r.Name] = true

		if r.Port < 1024 || r.Port > 65535 || r.Port == a.HealthPort {
			add("Routes[%d].Port %d: want the builder's registered port, 1024..65535 and not the health port", i, r.Port)
		}

		if ports[r.Port] {
			add("Routes[%d].Port %d is used twice", i, r.Port)
		}

		ports[r.Port] = true

		if listeners[r.Listener] {
			add("Routes[%d].Listener %q is used twice", i, r.Listener)
		}

		listeners[r.Listener] = true

		if !tagValRe.MatchString(r.TargetTagValue) {
			add("Routes[%d].TargetTagValue %q: want the builder's role tag value", i, r.TargetTagValue)
		}

		if targets[r.TargetTagValue] {
			add("Routes[%d].TargetTagValue %q is used twice: exactly one instance may match a route", i, r.TargetTagValue)
		}

		targets[r.TargetTagValue] = true
	}

	a.validateMetrics(add, ports, listeners)

	if !tagKeyRe.MatchString(a.RoleTagKey) {
		add("RoleTagKey %q: not a valid tag key", a.RoleTagKey)
	}

	// The statements, checked with a probe account so a bad key ARN or tag
	// fails at New and not in the middle of the policy.
	if _, err := a.statements(wakeproxyPolicyContext(probeAccount, "aws", a.Region)); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// validateMetrics checks the metrics listener against the routes' ports and
// listener names (the maps validate fills).
func (a ProxyArgs) validateMetrics(add func(string, ...any), ports map[int]bool, listeners map[string]bool) {
	if a.MetricsListenPort == 0 {
		if len(a.MetricsSources) != 0 || a.MetricsListener != "" {
			add("MetricsSources and MetricsListener are set without MetricsListenPort")
		}

		return
	}

	if a.MetricsListenPort < 1024 || a.MetricsListenPort > 65535 || a.MetricsListenPort == a.HealthPort || ports[a.MetricsListenPort] {
		add("MetricsListenPort %d: want the registered port, 1024..65535 and not the health port or a route's port", a.MetricsListenPort)
	}

	if listeners[a.MetricsListener] {
		add("MetricsListener %q is also a route's listener", a.MetricsListener)
	}

	if len(a.MetricsSources) == 0 {
		add("MetricsSources is empty: nobody could scrape the metrics; name the scraper's security group or range")
	}

	host, port, err := net.SplitHostPort(a.MetricsAddress)
	if err != nil || port != strconv.Itoa(a.MetricsListenPort) {
		add("MetricsAddress %q: with MetricsListenPort %d want host:%d", a.MetricsAddress, a.MetricsListenPort, a.MetricsListenPort)

		return
	}

	if ip, err := netip.ParseAddr(host); host == "localhost" || (err == nil && ip.IsLoopback()) {
		add("MetricsAddress %q is loopback: the NLB cannot reach it; leave it empty to bind 0.0.0.0", a.MetricsAddress)
	}
}

func (a ProxyArgs) selectors() []wakeproxy.Selector {
	out := make([]wakeproxy.Selector, len(a.Routes))
	for i, r := range a.Routes {
		out[i] = wakeproxy.Selector{TagKey: a.RoleTagKey, TagValue: r.TargetTagValue}
	}

	return out
}

func wakeproxyPolicyContext(account, partition, region string) asg.PolicyContext {
	return asg.PolicyContext{Account: account, Partition: partition, Region: region}
}

// statements are the proxy's own grants (wakeproxy.Statements): start, stop
// and tag of the routes' instances by tag, DescribeInstances, and the data
// keys.
func (a ProxyArgs) statements(pc asg.PolicyContext) ([]asg.Statement, error) {
	st, err := wakeproxy.Statements(wakeproxy.PolicyInput{
		Partition: pc.Partition, Region: pc.Region, Account: pc.Account,
		Targets: a.selectors(), KMSKeyARNs: a.KMSKeyARNs,
	})
	if err != nil {
		return nil, err
	}

	out := make([]asg.Statement, len(st))
	for i, s := range st {
		out[i] = asg.Statement(s)
	}

	return out, nil
}

// NewProxy creates the wake proxy of an environment: an appliance in the
// Auto Scaling group mode (desired 1 and one stopped warm-pool spare, with the
// launch hook) behind listeners of the shared NLB, one per builder. The
// builders are not targets of anything.
func NewProxy(ctx *pulumi.Context, name string, args *ProxyArgs, opts ...pulumi.ResourceOption) (*Proxy, error) {
	if args == nil {
		return nil, fmt.Errorf("buildkit ec2 proxy: NewProxy(%q, ...): args is nil", name)
	}

	a := args.withDefaults(name)
	if err := a.validate(); err != nil {
		return nil, fmt.Errorf("buildkit ec2 proxy: NewProxy(%q, ...): %w", name, err)
	}

	names := asg.Names{Base: a.Name}
	resolved := names.Resolve(name)

	rendered, mods, err := proxyUserData(a, resolved)
	if err != nil {
		return nil, fmt.Errorf("buildkit ec2 proxy: NewProxy(%q, ...): %w", name, err)
	}

	listeners := make([]front.Listener, len(a.Routes))
	for i, r := range a.Routes {
		listeners[i] = front.Listener{
			Name:                       r.Listener,
			Port:                       r.Port,
			TargetPort:                 r.Port,
			IdleTimeoutSeconds:         DefaultIdleTimeoutSeconds,
			DeregistrationDelaySeconds: DefaultDeregistrationDelaySeconds,
			HealthCheck:                front.HealthCheck{Protocol: "HTTP", Path: "/healthz", Port: a.HealthPort},
			Sources:                    a.Sources,
		}
	}

	// The metrics listener: the same wiring as a route. The health check is
	// the proxy's /healthz on the health port, like the routes': the group's
	// ELB health must follow the proxy, not the metrics server (a proxy whose
	// metrics fail still serves builds and must not be replaced for that).
	if a.MetricsListenPort != 0 {
		listeners = append(listeners, front.Listener{
			Name:                       a.MetricsListener,
			Port:                       a.MetricsListenPort,
			TargetPort:                 a.MetricsListenPort,
			DeregistrationDelaySeconds: DefaultDeregistrationDelaySeconds,
			HealthCheck:                front.HealthCheck{Protocol: "HTTP", Path: "/healthz", Port: a.HealthPort},
			Sources:                    a.MetricsSources,
		})
	}

	f, err := front.Attach(ctx, name, a.NLB, listeners, opts...)
	if err != nil {
		return nil, fmt.Errorf("buildkit ec2 proxy: NewProxy(%q, ...): %w", name, err)
	}

	box, err := asg.New(ctx, name, &asg.Args{
		Names:    names,
		Tags:     a.Tags,
		Provider: a.Provider,
		UserData: rendered,
		Image:    asg.ImageArgs{SSMParameter: imageParameters[a.Arch]},
		Instance: asg.InstanceArgs{
			Type:       a.InstanceType,
			HopLimit:   1,
			RootVolume: &asg.RootVolumeArgs{SizeGiB: a.RootVolumeGiB},
		},
		Group: asg.GroupArgs{
			Min: 1, Max: 2, Desired: 1,
			SubnetIDs:       a.SubnetIDs,
			HealthCheckType: "ELB",
			TargetGroupARNs: f.TargetGroupARNs(),
		},
		Hook:     &asg.HookArgs{HeartbeatTimeoutSeconds: DefaultProxyHookHeartbeatSeconds},
		WarmPool: &asg.WarmPoolArgs{MinSize: 1, ReuseOnScaleIn: true},
		Refresh:  &asg.RefreshArgs{MinHealthyPercentage: 100, MaxHealthyPercentage: 200},
		IAM: asg.IAMArgs{
			PermissionsBoundary: a.PermissionsBoundary,
			StatementsFunc: func(pc asg.PolicyContext) []asg.Statement {
				// Validated in NewProxy with a probe account: the same
				// checks, so the real account cannot fail them.
				st, _ := a.statements(pc)

				return st
			},
		},
		Security: asg.SecurityArgs{
			VPCID:       a.VPCID,
			Description: a.Name + " BuildKit wake proxy",
			Style:       asg.RulesSeparate,
			Ingress:     f.TargetIngress(),
		},
		Contribution: mods,
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("buildkit ec2 proxy: NewProxy(%q, ...): %w", name, err)
	}

	return &Proxy{
		Box: box, Front: f, Rendered: rendered, RoleTagKey: a.RoleTagKey,
		HealthPort: a.HealthPort, MetricsAddress: a.MetricsAddress, MetricsListenPort: a.MetricsListenPort,
		GroupName: resolved.Group,
	}, nil
}

// SecurityGroupID is the proxy's security group, the source of the builders'
// ingress rule (OnDemandArgs.ProxySecurityGroupID).
func (p *Proxy) SecurityGroupID() pulumi.StringOutput {
	return p.Box.SecurityGroup.ID().ToStringOutput()
}

func proxyUserData(a ProxyArgs, r asg.Resolved) (*bootstrap.Rendered, asg.Contribution, error) {
	routes := make([]wakeproxy.Route, len(a.Routes))
	for i, rt := range a.Routes {
		routes[i] = wakeproxy.Route{
			Name: rt.Name, ListenPort: rt.Port, TargetPort: DaemonPort,
			Target: wakeproxy.Selector{TagKey: a.RoleTagKey, TagValue: rt.TargetTagValue},
		}
	}

	wp := wakeproxy.Args{
		Binary: map[string]bootstrap.Source{a.Arch: {URL: wakeproxy.ReleaseURL(a.Version, a.Arch), SHA256: a.SHA256}},
		Region: a.Region, Routes: routes, HealthPort: a.HealthPort, MetricsAddress: a.MetricsAddress,
		IdleTimeout: a.IdleTimeout, WakeTimeout: a.WakeTimeout,
		// Account stays empty: the role's statements come from
		// IAM.StatementsFunc, which knows the account at deploy time.
	}

	// The proxy's own check (its health port) is contributed by the module;
	// a proxy that fails it for three minutes is restarted twice, then marked
	// Unhealthy so the group replaces it.
	wd := watchdog.Args{IntervalSeconds: 30, FailMinutes: 3, RestartAttempts: 2, RestartUnits: []string{wakeproxy.UnitName}}

	mods, err := asg.Collect(wakeproxy.New(wp), watchdog.New(wd))
	if err != nil {
		return nil, asg.Contribution{}, err
	}

	p, err := bootstrap.New(bootstrap.Options{Lifecycle: r.Lifecycle()})
	if err != nil {
		return nil, asg.Contribution{}, err
	}

	if err := mods.AddTo(p); err != nil {
		return nil, asg.Contribution{}, err
	}

	rendered, err := p.Render()
	if err != nil {
		return nil, asg.Contribution{}, err
	}

	return rendered, mods, nil
}
