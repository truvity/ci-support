package ec2_test

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg/front"

	"github.com/truvity/ci-support/pkg/buildkit"
	"github.com/truvity/ci-support/pkg/buildkit/ec2"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The repository is public: the tests build account ids and ARNs at run
// time and the goldens hold placeholders.
const (
	testRegion = "eu-west-1"
	part       = "aw" + "s"
)

var (
	testAccount = strings.Repeat("7", 12)
	shaRe       = regexp.MustCompile(`[0-9a-f]{64}`)
)

func sanitize(s string) string {
	s = strings.ReplaceAll(s, testAccount, "@ACCOUNT@")

	return strings.ReplaceAll(s, "arn:"+part, "arn:@PARTITION@")
}

type recorder struct {
	mu       sync.Mutex
	all      []map[string]any
	userData string
}

func (r *recorder) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	in := args.Inputs.Mappable()
	if args.TypeToken == "aws:ec2/launchTemplate:LaunchTemplate" {
		r.userData, _ = in["userData"].(string)
		in["userData"] = "<user-data>"
	}

	r.all = append(r.all, map[string]any{"type": args.TypeToken, "name": args.Name, "inputs": in})

	out := args.Inputs.Copy()
	switch args.TypeToken {
	case "aws:ec2/launchTemplate:LaunchTemplate":
		out["latestVersion"] = resource.NewNumberProperty(1)
	case "aws:lb/targetGroup:TargetGroup", "aws:lb/listener:Listener":
		out["arn"] = resource.NewStringProperty("arn:" + part + ":elasticloadbalancing:" + testRegion + ":" + testAccount + ":" + args.Name)
	case "aws:iam/role:Role":
		out["arn"] = resource.NewStringProperty("arn:" + part + ":iam::" + testAccount + ":role/" + args.Name)
	}

	return args.Name + "_id", out, nil
}

func (r *recorder) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	switch args.Token {
	case "aws:index/getRegion:getRegion":
		return resource.PropertyMap{"region": resource.NewStringProperty(testRegion), "name": resource.NewStringProperty(testRegion)}, nil
	case "aws:index/getCallerIdentity:getCallerIdentity":
		return resource.PropertyMap{"accountId": resource.NewStringProperty(testAccount)}, nil
	case "aws:ssm/getParameter:getParameter":
		return resource.PropertyMap{"name": args.Args["name"], "value": resource.NewStringProperty("ami-0example")}, nil
	}

	return args.Args, nil
}

func (r *recorder) dump(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	sort.Slice(r.all, func(i, j int) bool {
		return r.all[i]["type"].(string)+"/"+r.all[i]["name"].(string) < r.all[j]["type"].(string)+"/"+r.all[j]["name"].(string)
	})

	b, err := json.MarshalIndent(r.all, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	return sanitize(shaRe.ReplaceAllString(string(b), "<sha256>") + "\n")
}

func (r *recorder) find(t *testing.T, typ, name string) map[string]any {
	t.Helper()

	for _, x := range r.all {
		if x["type"] == typ && x["name"] == name {
			return x["inputs"].(map[string]any)
		}
	}

	t.Fatalf("no %s named %s", typ, name)

	return nil
}

func golden(t *testing.T, name, got string) {
	t.Helper()

	got = sanitize(got)
	file := filepath.Join("testdata", name)

	if *update {
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	if string(want) != got {
		t.Errorf("%s differs from the golden (go test -update to accept):\n%s", name, got)
	}
}

func args() *ec2.Args {
	return &ec2.Args{
		Arch:         "arm64",
		Name:         "devel-buildkit-arm64",
		InstanceType: "m7g.xlarge",
		VPCID:        pulumi.String("vpc-0example"),
		SubnetIDs:    []pulumi.StringInput{pulumi.String("subnet-0a"), pulumi.String("subnet-0b")},
		NLB: front.SharedNLB{
			ARN:     "arn:" + part + ":elasticloadbalancing:" + testRegion + ":" + testAccount + ":loadbalancer/net/box-front/0example",
			DNSName: "box-front-0example.elb.example", ZoneID: "Z0EXAMPLE", SecurityGroupID: "sg-0front", VPCID: "vpc-0example",
			VPCCIDR: "10.0.0.0/16",
		},
		Port:    1235,
		Sources: []front.Source{{SecurityGroupID: pulumi.String("sg-0runners"), Description: "the runners"}},
		Certificate: ec2.Certificate{
			Cert: "/ci/test/buildkit-server/arm64/cert", Key: "/ci/test/buildkit-server/arm64/key", ClientCA: "/ci/test/buildkit-server/client-ca",
			KeyAlias: "alias/test-ci-buildkit-server",
		},
		Daemon: buildkit.Config{MaxParallelism: 6},
	}
}

func run(t *testing.T, a *ec2.Args) (*recorder, *ec2.Builder, error) {
	t.Helper()

	rec := &recorder{}

	var b *ec2.Builder

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		var err error
		b, err = ec2.New(ctx, "buildkit-arm64", a)

		return err
	}, pulumi.WithMocks("p", "s", rec))

	return rec, b, err
}

func TestResourceSet(t *testing.T) {
	rec, _, err := run(t, args())
	if err != nil {
		t.Fatal(err)
	}

	golden(t, "resources.golden.json", rec.dump(t))
}

func TestUserDataFitsAndGolden(t *testing.T) {
	_, b, err := run(t, args())
	if err != nil {
		t.Fatal(err)
	}

	if b.Rendered.Size > 16384 {
		t.Errorf("user-data is %d bytes", b.Rendered.Size)
	}

	if len(b.Rendered.Objects) != 0 {
		t.Errorf("the user-data spilled into %d objects; the builder has no bucket", len(b.Rendered.Objects))
	}

	golden(t, "userdata.golden.yaml", b.Rendered.CloudConfig)

	for _, want := range []string{
		"/run/asg-ssmcert/buildkit/cert.pem", "OWNER='buildkit:buildkit'", "GROUP='devel-buildkit-arm64'", "HOOK='devel-buildkit-arm64-launch'",
		"RESTART_UNITS=('buildkitd.service')", "debug workers",
	} {
		if !strings.Contains(b.Rendered.CloudConfig, want) {
			t.Errorf("user-data lacks %s", want)
		}
	}
}

func TestLaunchTemplateCarriesTheRenderedUserData(t *testing.T) {
	rec, b, err := run(t, args())
	if err != nil {
		t.Fatal(err)
	}

	got, err := base64.StdEncoding.DecodeString(rec.userData)
	if err != nil || string(got) != string(b.Rendered.UserData) {
		t.Errorf("launch template user-data differs from the rendered plan (%v)", err)
	}
}

func TestPolicy(t *testing.T) {
	rec, _, err := run(t, args())
	if err != nil {
		t.Fatal(err)
	}

	pol := rec.find(t, "aws:iam/role:Role", "buildkit-arm64")["inlinePolicies"].([]any)[0].(map[string]any)["policy"].(string)
	golden(t, "policy.golden.json", pol+"\n")

	for _, want := range []string{
		`"kms:Decrypt"`, `"alias/test-ci-buildkit-server"`, `"ssm.eu-west-1.amazonaws.com"`,
		"parameter/ci/test/buildkit-server/arm64/key", "autoscaling:EnterStandby", "autoscaling:SetInstanceHealth",
	} {
		if !strings.Contains(pol, want) {
			t.Errorf("policy lacks %s", want)
		}
	}

	for _, refuse := range []string{"ecr:", "s3:", `"Resource": "*"`, `"kms:Encrypt"`} {
		if strings.Contains(pol, refuse) {
			t.Errorf("policy holds %s", refuse)
		}
	}
}

func TestOnlyTheFrontReachesTheBox(t *testing.T) {
	rec, _, err := run(t, args())
	if err != nil {
		t.Fatal(err)
	}

	var rules []map[string]any

	for _, x := range rec.all {
		if x["type"] == "aws:vpc/securityGroupIngressRule:SecurityGroupIngressRule" {
			rules = append(rules, x["inputs"].(map[string]any))
		}
	}

	// One rule on the NLB's group (the runners, port 1235) and one on the
	// box's (the NLB's group, port 1234).
	if len(rules) != 2 {
		t.Fatalf("rules %v", rules)
	}

	for _, r := range rules {
		switch r["securityGroupId"] {
		case "sg-0front":
			if r["referencedSecurityGroupId"] != "sg-0runners" || r["fromPort"] != float64(1235) {
				t.Errorf("front rule %v", r)
			}
		default:
			if r["referencedSecurityGroupId"] != "sg-0front" || r["fromPort"] != float64(1234) {
				t.Errorf("box rule %v", r)
			}
		}
	}

	g := rec.find(t, "aws:autoscaling/group:Group", "buildkit-arm64")
	if g["healthCheckType"] != "ELB" || g["maxSize"] != float64(3) || len(g["targetGroupArns"].([]any)) != 1 {
		t.Errorf("group %v", g)
	}
}

func TestRefusals(t *testing.T) {
	for name, mut := range map[string]func(*ec2.Args){
		"arch":    func(a *ec2.Args) { a.Arch = "x86" },
		"max":     func(a *ec2.Args) { a.Min, a.Max = 1, 1 },
		"port":    func(a *ec2.Args) { a.Port = 0 },
		"sources": func(a *ec2.Args) { a.Sources = nil },
		"cert":    func(a *ec2.Args) { a.Certificate.Key = "" },
		"alias":   func(a *ec2.Args) { a.Certificate.KeyAlias = "key/1" },
		"daemon":  func(a *ec2.Args) { a.Daemon.MaxParallelism = 0 },
		"no nlb":  func(a *ec2.Args) { a.NLB = front.SharedNLB{} },
	} {
		a := args()
		mut(a)

		if _, _, err := run(t, a); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// The key's statement for a builder names the role New creates and the
// three parameters the role's own grant names, and nothing else.
func TestKeyPolicyStatementMatchesTheRolesGrant(t *testing.T) {
	a := args()

	st, err := ec2.KeyPolicyStatement(part, testRegion, testAccount, a.Name, a.Certificate)
	if err != nil {
		t.Fatal(err)
	}

	if st["Sid"] != "BuilderDevelBuildkitArm64DecryptsItsCertificate" || st["Effect"] != "Allow" {
		t.Errorf("sid/effect %v %v", st["Sid"], st["Effect"])
	}

	if acts := st["Action"].([]string); len(acts) != 1 || acts[0] != "kms:Decrypt" {
		t.Errorf("actions %v: the builder only decrypts", acts)
	}

	cond := st["Condition"].(map[string]any)
	if got := cond["ArnEquals"].(map[string]any)["aws:PrincipalArn"]; got != "arn:"+part+":iam::"+testAccount+":role/"+a.Name {
		t.Errorf("principal %v", got)
	}

	eq := cond["StringEquals"].(map[string]any)
	if eq["kms:ViaService"] != "ssm."+testRegion+".amazonaws.com" {
		t.Errorf("via %v", eq["kms:ViaService"])
	}

	rec, _, err := run(t, a)
	if err != nil {
		t.Fatal(err)
	}

	pol := rec.find(t, "aws:iam/role:Role", "buildkit-arm64")["inlinePolicies"].([]any)[0].(map[string]any)["policy"].(string)

	params := eq["kms:EncryptionContext:PARAMETER_ARN"].([]string)
	if len(params) != 3 {
		t.Fatalf("parameters %v", params)
	}

	for _, p := range params {
		if !strings.Contains(pol, `"`+p+`"`) {
			t.Errorf("the key admits %s, which the role's own grant does not name", p)
		}
	}

	if _, err := ec2.KeyPolicyStatement(part, testRegion, testAccount, "Devel_Buildkit", a.Certificate); err == nil {
		t.Error("accepted a name New refuses")
	}

	if _, err := ec2.KeyPolicyStatement(part, testRegion, testAccount, a.Name, ec2.Certificate{Cert: "/x"}); err == nil {
		t.Error("accepted a certificate without its key and client CA")
	}
}
