package api

import (
	"bytes"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
)

func httpsTask() *heronv1.ProbeTask {
	return &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_HTTP, Target: "https://example.com/", IntervalS: 60, TimeoutMs: 1000}
}

func pinBytes(b byte) []byte {
	return bytes.Repeat([]byte{b}, 32)
}

func savedConfig(t *testing.T, task *heronv1.ProbeTaskDetail) []byte {
	t.Helper()
	if task.GetTask().GetConfigId() == nil || len(task.GetTask().GetConfigId()) != 16 || len(task.GetTask().GetCertSpkiSha256()) > 0 && len(task.GetTask().GetCertSpkiSha256()) != 32 {
		t.Fatalf("saved task identity/pin = %x / %x", task.GetTask().GetConfigId(), task.GetTask().GetCertSpkiSha256())
	}
	return task.GetTask().GetConfigId()
}

func TestCertPinSaveKeepsPinUnlessTheActionSaysOtherwise(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	created, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task: httpsTask(), NodeIds: []int64{id},
		CertPin: &heronv1.CertPinChange{Action: &heronv1.CertPinChange_SetSpkiSha256{SetSpkiSha256: pinBytes(1)}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	cfg := savedConfig(t, created.Msg.Task)
	if !bytes.Equal(created.Msg.Task.Task.CertSpkiSha256, pinBytes(1)) {
		t.Fatalf("pin = %x", created.Msg.Task.Task.CertSpkiSha256)
	}
	// 只改间隔、不带 cert_pin：pin 与身份都留着。
	edited := proto.Clone(created.Msg.Task.Task).(*heronv1.ProbeTask)
	edited.IntervalS = 120
	edited.CertSpkiSha256 = pinBytes(9) // 只输出，必须被忽略
	edited.ConfigId = bytes.Repeat([]byte{7}, 16)
	again, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task: edited, NodeIds: []int64{id}, ExpectedConfigId: cfg,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Msg.Task.Task.CertSpkiSha256, pinBytes(1)) || bytes.Equal(again.Msg.Task.Task.ConfigId, cfg) {
		t.Fatalf("interval edit pin=%x config changed=%v", again.Msg.Task.Task.CertSpkiSha256, !bytes.Equal(again.Msg.Task.Task.ConfigId, cfg))
	}
	// 内容没变的保存保留身份。
	same, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task: proto.Clone(again.Msg.Task.Task).(*heronv1.ProbeTask), NodeIds: []int64{id}, ExpectedConfigId: again.Msg.Task.Task.ConfigId,
	}))
	if err != nil || !bytes.Equal(same.Msg.Task.Task.ConfigId, again.Msg.Task.Task.ConfigId) || !bytes.Equal(same.Msg.Task.Task.CertSpkiSha256, pinBytes(1)) {
		t.Fatalf("unchanged save = %v %v", same, err)
	}
}

func TestCertPinRejectsEmptySetUnsetOneofAndStalePrecondition(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	created, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: httpsTask(), NodeIds: []int64{id}}))
	if err != nil {
		t.Fatal(err)
	}
	task := created.Msg.Task.Task
	for _, tc := range []struct {
		name string
		req  *heronv1.SaveProbeTaskRequest
		text string
	}{
		{"empty set", &heronv1.SaveProbeTaskRequest{Task: task, NodeIds: []int64{id}, CertPin: &heronv1.CertPinChange{Action: &heronv1.CertPinChange_SetSpkiSha256{}}}, "must not be empty"},
		{"unset oneof", &heronv1.SaveProbeTaskRequest{Task: task, NodeIds: []int64{id}, CertPin: &heronv1.CertPinChange{}}, "set_spki_sha256 or clear is required"},
		{"create with expected", &heronv1.SaveProbeTaskRequest{Task: httpsTask(), NodeIds: []int64{id}, ExpectedConfigId: task.ConfigId, CertPin: &heronv1.CertPinChange{Action: &heronv1.CertPinChange_SetSpkiSha256{SetSpkiSha256: pinBytes(1)}}}, "must not be set when creating"},
		{"set without expected", &heronv1.SaveProbeTaskRequest{Task: task, NodeIds: []int64{id}, CertPin: &heronv1.CertPinChange{Action: &heronv1.CertPinChange_SetSpkiSha256{SetSpkiSha256: pinBytes(1)}}}, "required when setting"},
		{"stale expected", &heronv1.SaveProbeTaskRequest{Task: task, NodeIds: []int64{id}, ExpectedConfigId: bytes.Repeat([]byte{9}, 16), CertPin: &heronv1.CertPinChange{Action: &heronv1.CertPinChange_SetSpkiSha256{SetSpkiSha256: pinBytes(1)}}}, "does not match"},
		{"http pin", &heronv1.SaveProbeTaskRequest{Task: &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_HTTP, Target: "http://example.com/", IntervalS: 60, TimeoutMs: 1000}, NodeIds: []int64{id}, CertPin: &heronv1.CertPinChange{Action: &heronv1.CertPinChange_SetSpkiSha256{SetSpkiSha256: pinBytes(1)}}}, "https"},
		{"icmp pin", &heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{id}, CertPin: &heronv1.CertPinChange{Action: &heronv1.CertPinChange_SetSpkiSha256{SetSpkiSha256: pinBytes(1)}}}, "https"},
		{"short pin", &heronv1.SaveProbeTaskRequest{Task: httpsTask(), NodeIds: []int64{id}, CertPin: &heronv1.CertPinChange{Action: &heronv1.CertPinChange_SetSpkiSha256{SetSpkiSha256: pinBytes(1)[:31]}}}, "exactly 32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(tc.req))
			want := connect.CodeInvalidArgument
			if tc.name == "stale expected" {
				want = connect.CodeFailedPrecondition
			}
			if connect.CodeOf(err) != want || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("err=%v, want %v containing %q", err, want, tc.text)
			}
		})
	}
	cleared, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task: &heronv1.ProbeTask{Id: task.Id}, CertPin: &heronv1.CertPinChange{Action: &heronv1.CertPinChange_Clear{Clear: &emptypb.Empty{}}},
	}))
	if err != nil || len(cleared.Msg.Task.Task.CertSpkiSha256) != 0 || cleared.Msg.Task.Task.IntervalS != task.IntervalS {
		t.Fatalf("clear-only = %v %v", cleared, err)
	}
}

func TestExecuteChangeCarriesPinActionOutsideTheMask(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	created, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task: httpsTask(), NodeIds: []int64{id},
		CertPin: &heronv1.CertPinChange{Action: &heronv1.CertPinChange_SetSpkiSha256{SetSpkiSha256: pinBytes(2)}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	task := created.Msg.Task.Task
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{id}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	// 只改间隔：掩码不带 pin，基线里的 pin 必须留下，身份因为内容变了要换。
	edit := &heronv1.ExecuteChangeRequest{
		RequestId: "interval", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"task.interval_s"}},
		Change: &heronv1.ExecuteChangeRequest_SaveProbeTask{SaveProbeTask: &heronv1.SaveProbeTaskRequest{
			Task: &heronv1.ProbeTask{Id: task.Id, IntervalS: 90}, ExpectedConfigId: task.ConfigId,
		}},
	}
	previewChange(t, client, edit)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(edit)); err != nil {
		t.Fatal(err)
	}
	list, err := h.admin.ListProbeTasks(t.Context(), connect.NewRequest(&heronv1.ListProbeTasksRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var got *heronv1.ProbeTask
	for _, d := range list.Msg.Tasks {
		if d.Task.Id == task.Id {
			got = d.Task
		}
	}
	if got == nil || !bytes.Equal(got.CertSpkiSha256, pinBytes(2)) || bytes.Equal(got.ConfigId, task.ConfigId) || got.IntervalS != 90 {
		t.Fatalf("after interval edit: %v", got)
	}
	// 只做 set，空掩码。旧前置条件拒绝；当前身份接受。
	stale := &heronv1.ExecuteChangeRequest{
		RequestId: "stale-pin", Preview: true,
		Change: &heronv1.ExecuteChangeRequest_SaveProbeTask{SaveProbeTask: &heronv1.SaveProbeTaskRequest{
			Task: &heronv1.ProbeTask{Id: task.Id}, ExpectedConfigId: task.ConfigId,
			CertPin: &heronv1.CertPinChange{Action: &heronv1.CertPinChange_SetSpkiSha256{SetSpkiSha256: pinBytes(3)}},
		}},
	}
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(stale)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale precondition err=%v", err)
	}
	fresh := proto.Clone(stale).(*heronv1.ExecuteChangeRequest)
	fresh.Preview = false
	fresh.RequestId = "fresh-pin"
	fresh.GetSaveProbeTask().ExpectedConfigId = got.ConfigId
	previewChange(t, client, fresh)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(fresh)); err != nil {
		t.Fatal(err)
	}
	list, err = h.admin.ListProbeTasks(t.Context(), connect.NewRequest(&heronv1.ListProbeTasksRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list.Msg.Tasks {
		if d.Task.Id == task.Id && !bytes.Equal(d.Task.CertSpkiSha256, pinBytes(3)) {
			t.Fatalf("pin after action-only set = %x", d.Task.CertSpkiSha256)
		}
	}
	// 空掩码且没有动作仍拒绝。
	empty := &heronv1.ExecuteChangeRequest{RequestId: "empty", Preview: true, Change: &heronv1.ExecuteChangeRequest_SaveProbeTask{SaveProbeTask: &heronv1.SaveProbeTaskRequest{Task: &heronv1.ProbeTask{Id: task.Id}}}}
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(empty)); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "editable field or action") {
		t.Fatalf("empty mask err=%v", err)
	}
	_ = heronv1connect.AdminServiceListProbeCertificatesProcedure
}

func TestListProbeCertificatesHidesOutOfScopeTasks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, _ := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	saved, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: httpsTask(), NodeIds: []int64{a, b}}))
	if err != nil {
		t.Fatal(err)
	}
	narrow, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{a}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	if _, err := narrow.ListProbeCertificates(t.Context(), connect.NewRequest(&heronv1.ListProbeCertificatesRequest{TaskId: saved.Msg.Task.Task.Id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("partial scope err=%v, want NotFound", err)
	}
	own, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{a}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	// 任务只分配给 a 时，a 的 token 可以标注，也只能看到 a。
	only, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: httpsTask(), NodeIds: []int64{a}}))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := own.ListProbeCertificates(t.Context(), connect.NewRequest(&heronv1.ListProbeCertificatesRequest{TaskId: only.Msg.Task.Task.Id}))
	if err != nil || len(resp.Msg.Nodes) != 1 || resp.Msg.Nodes[0].NodeId != a {
		t.Fatalf("scoped list = %v %v", resp, err)
	}
	if heronv1.File_heron_v1_public_proto.Services().ByName("PublicService").Methods().ByName("ListProbeCertificates") != nil {
		t.Fatal("public service exposes certificate candidates")
	}
}

func TestSaveProbeTaskFieldClassificationIsExhaustive(t *testing.T) {
	t.Parallel()
	var check func(prefix string, md protoreflect.MessageDescriptor)
	check = func(prefix string, md protoreflect.MessageDescriptor) {
		t.Helper()
		for i := 0; i < md.Fields().Len(); i++ {
			f := md.Fields().Get(i)
			path := prefix + string(f.Name())
			class, ok := saveProbeTaskClass[path]
			if !ok {
				t.Errorf("%s is not classified", path)
				continue
			}
			if f.Message() != nil && !f.IsList() && !f.IsMap() && class == "descend" {
				check(path+".", f.Message())
			}
		}
	}
	check("", (&heronv1.SaveProbeTaskRequest{}).ProtoReflect().Descriptor())
	for path, class := range saveProbeTaskClass {
		if class == "" {
			t.Errorf("empty class for %s", path)
		}
	}
}
