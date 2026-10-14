package api

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

// 存储类别与协议枚举必须一一对应：漏一个映射会让 ListAlertEvents 整页 internal，
// 两个类别映到同一枚举值会让只读口径分不清它们。
func TestDeliveryFailureMappingIsOneToOne(t *testing.T) {
	t.Parallel()
	seen := map[heronv1.DeliveryFailure]store.DeliveryFailure{}
	for _, f := range append([]store.DeliveryFailure{store.FailureNone}, store.DeliveryFailures()...) {
		v, err := deliveryFailureProto(f)
		if err != nil {
			t.Fatalf("%q has no protocol value: %v", f, err)
		}
		if prev, dup := seen[v]; dup {
			t.Fatalf("%q and %q both map to %v", prev, f, v)
		}
		seen[v] = f
	}
	if got, want := len(seen), len(heronv1.DeliveryFailure_name); got != want {
		t.Fatalf("store categories cover %d of %d protocol values: %v", got, want, seen)
	}
	if v, err := deliveryFailureProto("bogus"); err == nil {
		t.Fatalf("unknown category mapped to %v instead of failing", v)
	}
	if seen[heronv1.DeliveryFailure_DELIVERY_FAILURE_UNSPECIFIED] != store.FailureNone {
		t.Fatal("UNSPECIFIED must mean no failure")
	}
}

// rawBody 用纯 HTTP+JSON 调一个 AdminService 方法并返回整个响应体，断言针对的是序列化后真正出线的字节。
func rawBody(t *testing.T, h *harness, method, body string, headers map[string][]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/heron.v1.AdminService/"+method, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestDeliveryErrorTextOnlyReachesSessions(t *testing.T) {
	t.Parallel()
	const echoed = `{"text":"alert","token":"secret-echo-7f3a"}`
	h := newHarness(t, "")
	h.login(t)
	_, tok := createToken(t, h, "reader")
	n, _ := h.createNode(t, "n")
	r := saveRule(t, h, offlineRule())
	c := saveChannel(t, h, webhook("http://127.0.0.1"))
	ev, err := h.store.RecordTransition(t.Context(), r.Id, n, store.StateFiring, "", time.Time{}, store.AlertEvent{Transition: store.TransitionFiring, At: h.clk.Now(), Summary: "down"}, []store.DeliveryTarget{{ChannelID: c.Id}})
	if err != nil {
		t.Fatal(err)
	}
	id := ev.Deliveries[0].ID
	if err := h.store.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, store.DeliveryResult{Done: true, Failure: store.FailureHTTPStatus, HTTPStatus: 401, Error: echoed}); err != nil {
		t.Fatal(err)
	}

	status, body := rawBody(t, h, "ListAlertEvents", "{}", bearer(tok))
	if status != http.StatusOK || strings.Contains(body, "secret-echo-7f3a") {
		t.Fatalf("token ListAlertEvents status=%d leaked error text: %s", status, body)
	}
	if !strings.Contains(body, `"failure":"DELIVERY_FAILURE_HTTP_STATUS"`) || !strings.Contains(body, `"httpStatus":401`) {
		t.Fatalf("token ListAlertEvents lacks category or status: %s", body)
	}

	resp, err := h.admin.GetAlertDeliveryError(t.Context(), connect.NewRequest(&heronv1.GetAlertDeliveryErrorRequest{DeliveryId: id}))
	if err != nil || resp.Msg.GetError() != echoed {
		t.Fatalf("session GetAlertDeliveryError=%v err=%v, want %q", resp, err, echoed)
	}

	got := rawCall(t, h, "GetAlertDeliveryError", `{"deliveryId":"`+strconv.FormatInt(id, 10)+`"}`, bearer(tok))
	if got.status != http.StatusForbidden || got.code != "permission_denied" {
		t.Fatalf("token GetAlertDeliveryError=%+v, want 403 permission_denied", got)
	}
}

func TestListAlertEventsReportsFailureCategory(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	n, _ := h.createNode(t, "n")
	r := saveRule(t, h, offlineRule())
	c := saveChannel(t, h, webhook("http://127.0.0.1"))
	results := []store.DeliveryResult{
		{Done: true, Failure: store.FailureTransport, Error: "Post: connection refused"},
		{Done: true, Failure: store.FailureChannelDeleted},
		{},
	}
	channels := make([]store.DeliveryTarget, len(results))
	for i := range channels {
		channels[i] = store.DeliveryTarget{ChannelID: c.Id}
	}
	ev, err := h.store.RecordTransition(t.Context(), r.Id, n, store.StateFiring, "", time.Time{}, store.AlertEvent{Transition: store.TransitionFiring, At: h.clk.Now()}, channels)
	if err != nil {
		t.Fatal(err)
	}
	for i, res := range results {
		if res.Failure == store.FailureNone {
			continue
		}
		if err := h.store.UpdateBatch(t.Context(), ev.Deliveries[i].BatchID, res); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := h.admin.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	ds := resp.Msg.GetEvents()[0].GetDeliveries()
	for i, want := range []heronv1.DeliveryFailure{heronv1.DeliveryFailure_DELIVERY_FAILURE_TRANSPORT, heronv1.DeliveryFailure_DELIVERY_FAILURE_CHANNEL_DELETED, heronv1.DeliveryFailure_DELIVERY_FAILURE_UNSPECIFIED} {
		if d := ds[i]; d.GetId() != ev.Deliveries[i].ID || d.GetFailure() != want || d.HttpStatus != nil {
			t.Errorf("delivery %d = %v, want id %d failure %v without http_status", i, d, ev.Deliveries[i].ID, want)
		}
	}
}

func TestGetAlertDeliveryErrorNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	_, err := h.admin.GetAlertDeliveryError(t.Context(), connect.NewRequest(&heronv1.GetAlertDeliveryErrorRequest{DeliveryId: 999}))
	if codeOf(err) != connect.CodeNotFound || err.Error() != "not_found: delivery_id: alert delivery 999 does not exist" {
		t.Fatalf("err=%v", err)
	}
}
