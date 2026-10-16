package api

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// readMethods 是对 API token 开放的全部方法。把一个方法改成 ACCESS_READ 是在扩大
// token 的权限，必须同时改这份清单——与 Public* 字段允许列表同一口径。
var readMethods = []string{
	"ListNodes", "GetRegisterWindow", "GetSnapshot", "QueryMetrics", "GetTraffic",
	"ListProbeTasks", "QueryProbes", "ListAlertRules", "ListAlertEvents",
	"GetSettings", "GetBackupStatus", "GetStorageStats", "GetApiReference", "ListTags",
	"GetUpdates", "ListOperations", "ListNotifyChannelRefs", "ListSilences", "GetHeartbeatStatus", "ListRestoreRecords",
	"ListProbeComparisonNodes", "QueryProbeComparison", "ListProbeCertificates", "GetCaller",
}

func adminService() protoreflect.ServiceDescriptor {
	return heronv1.File_heron_v1_admin_proto.Services().ByName("AdminService")
}

func TestAdminAccessTableMatchesDeclaredPolicy(t *testing.T) {
	t.Parallel()
	svc := adminService()
	table := accessTable(svc)
	if len(table) != svc.Methods().Len() {
		t.Fatalf("table has %d entries for %d methods", len(table), svc.Methods().Len())
	}
	read := map[string]bool{}
	for _, m := range readMethods {
		read[m] = true
	}
	for i := 0; i < svc.Methods().Len(); i++ {
		name := string(svc.Methods().Get(i).Name())
		want := heronv1.Access_ACCESS_SESSION
		switch {
		case name == "ExecuteChange":
			want = heronv1.Access_ACCESS_CHANGE
		case name == "Login" || name == "BeginPasskeyLogin" || name == "FinishPasskeyLogin":
			want = heronv1.Access_ACCESS_LOGIN
		case read[name]:
			want = heronv1.Access_ACCESS_READ
		}
		if got := table["/heron.v1.AdminService/"+name]; got != want {
			t.Errorf("%s: access %v, want %v", name, got, want)
		}
		delete(read, name)
	}
	if len(read) != 0 {
		t.Errorf("readMethods names methods that do not exist: %v", read)
	}
}

// syntheticService 造一个只有一个方法的服务；opts 为 nil 表示该方法没有任何选项。
func syntheticService(t *testing.T, opts *descriptorpb.MethodOptions) protoreflect.ServiceDescriptor {
	t.Helper()
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:        proto.String("synthetic.proto"),
		Package:     proto.String("synthetic"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("M")}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("S"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name: proto.String("Bare"), InputType: proto.String(".synthetic.M"), OutputType: proto.String(".synthetic.M"), Options: opts,
			}},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Services().Get(0)
}

func expectPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("no panic; want one mentioning %q", want)
		}
		if !strings.Contains(r.(string), want) {
			t.Fatalf("panic %q does not mention %q", r, want)
		}
	}()
	fn()
}

func TestAccessTableRefusesUndeclaredOrUnknownAccess(t *testing.T) {
	t.Parallel()
	expectPanic(t, "synthetic.S.Bare", func() { accessTable(syntheticService(t, nil)) })
	unknown := &descriptorpb.MethodOptions{}
	proto.SetExtension(unknown, heronv1.E_Access, heronv1.Access(99))
	expectPanic(t, "synthetic.S.Bare", func() { accessTable(syntheticService(t, unknown)) })
	declared := &descriptorpb.MethodOptions{}
	proto.SetExtension(declared, heronv1.E_Access, heronv1.Access_ACCESS_READ)
	if got := accessTable(syntheticService(t, declared))["/synthetic.S/Bare"]; got != heronv1.Access_ACCESS_READ {
		t.Fatalf("declared method: %v", got)
	}
}
