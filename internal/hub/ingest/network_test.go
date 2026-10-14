package ingest

import (
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func TestNetworkReportAdmissionAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		result      *heronv1.AddressDetection
		ipv6, valid bool
	}{
		{"old", nil, false, true},
		{"unknown", &heronv1.AddressDetection{}, false, true},
		{"v4", &heronv1.AddressDetection{State: 1, Address: "8.8.4.4", CheckedAt: 100}, false, true},
		{"v6", &heronv1.AddressDetection{State: 1, Address: "2606:4700::1111", CheckedAt: 100}, true, true},
		{"unsupported", &heronv1.AddressDetection{State: 2, CheckedAt: 100}, true, true},
		{"failed", &heronv1.AddressDetection{State: 3, CheckedAt: 100}, false, true},
		{"disabled", &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_DISABLED, CheckedAt: 100}, true, true},
		{"disabled_address", &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_DISABLED, Address: "2606:4700::1111", CheckedAt: 100}, true, false},
		{"disabled_time", &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_DISABLED}, false, false},
		{"unknown_address", &heronv1.AddressDetection{Address: "8.8.4.4"}, false, false},
		{"unknown_time", &heronv1.AddressDetection{CheckedAt: 100}, false, false},
		{"missing_time", &heronv1.AddressDetection{State: 1, Address: "8.8.4.4"}, false, false},
		{"negative_time", &heronv1.AddressDetection{State: 2, CheckedAt: -1}, false, false},
		{"max_time", &heronv1.AddressDetection{State: 2, CheckedAt: 253402300799}, false, true},
		{"overflow_time", &heronv1.AddressDetection{State: 2, CheckedAt: 253402300800}, false, false},
		{"int64_time", &heronv1.AddressDetection{State: 2, CheckedAt: 9223372036854775807}, true, false},
		{"bad_enum", &heronv1.AddressDetection{State: 77, CheckedAt: 100}, false, false},
		{"failed_address", &heronv1.AddressDetection{State: 3, Address: "8.8.4.4", CheckedAt: 100}, false, false},
		{"unsupported_address", &heronv1.AddressDetection{State: 2, Address: "8.8.4.4", CheckedAt: 100}, false, false},
		{"wrong_family", &heronv1.AddressDetection{State: 1, Address: "8.8.4.4", CheckedAt: 100}, true, false},
		{"private", &heronv1.AddressDetection{State: 1, Address: "10.0.0.1", CheckedAt: 100}, false, false},
		{"bad_text", &heronv1.AddressDetection{State: 1, Address: "not-ip", CheckedAt: 100}, false, false},
		{"mapped", &heronv1.AddressDetection{State: 1, Address: "::ffff:8.8.4.4", CheckedAt: 100}, true, false},
		{"zone", &heronv1.AddressDetection{State: 1, Address: "2606:4700::1111%eth0", CheckedAt: 100}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t)
			id, tok := h.node(t)
			network := &heronv1.NetworkInfo{}
			if tc.ipv6 {
				network.Ipv6 = tc.result
			} else {
				network.Ipv4 = tc.result
			}
			req := report(tok, &heronv1.Metrics{})
			req.Msg.Facts = &heronv1.Facts{Network: network}
			req.Msg.FactsHash = 42
			_, err := h.client.Report(t.Context(), req)
			if !tc.valid {
				if connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Fatalf("invalid network admitted: %v", err)
				}
				if _, ok := h.live.Get(id); ok {
					t.Fatal("rejected network updated live")
				}
				n, e := h.store.GetNode(t.Context(), id)
				if e != nil || n.Facts != nil {
					t.Fatalf("rejected facts stored=%v %v", n.Facts, e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { m, _ := h.store.FactsHashes(t.Context()); return m[id] == 42 })
			n, err := h.store.GetNode(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if tc.result != nil && !proto.Equal(n.Facts.Network, network) {
				t.Fatalf("network=%v want=%v", n.Facts.Network, network)
			}
			if n.LastSource == "8.8.4.4" || n.CountryIP != "" {
				t.Fatalf("self-reported address became trusted source: %+v", n)
			}
		})
	}
}
