package main

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/Compaurum/gps_tracker_backend/lambda/ingest/protocol"
)

func TestPositionItemKeys(t *testing.T) {
	av, err := attributevalue.MarshalMap(positionItem{
		DeviceID:   "tracker-1",
		Fix:        protocol.Fix{Timestamp: 1760949990000, Lat: -32.9, Lon: 151.7},
		ReceivedAt: 1760950000123,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := av["deviceId"].(*types.AttributeValueMemberS); !ok || v.Value != "tracker-1" {
		t.Fatalf("deviceId: %#v", av["deviceId"])
	}
	if v, ok := av["ts"].(*types.AttributeValueMemberN); !ok || v.Value != "1760949990000" {
		t.Fatalf("ts sort key: %#v", av["ts"])
	}
	for _, absent := range []string{"Fix", "alt", "spd", "seq", "expiresAt"} {
		if _, ok := av[absent]; ok {
			t.Errorf("unexpected attribute %q", absent)
		}
	}
}

func TestNewerOnly(t *testing.T) {
	batteryVoltage := 3.9
	cases := map[string]struct {
		u        expression.UpdateBuilder
		wantSets []string
	}{
		"version only": {expression.UpdateBuilder{}, []string{"lastSeenAt"}},
		"with fields":  {setIfPresent(expression.UpdateBuilder{}, "batV", &batteryVoltage), []string{"batV", "lastSeenAt"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			expr, err := newerOnly("lastSeenAt", 1760950000123, tc.u)
			if err != nil {
				t.Fatal(err)
			}
			names := map[string]string{}
			for k, v := range expr.Names() {
				names[k] = v
			}
			resolve := func(s string) string {
				for k, v := range names {
					s = strings.ReplaceAll(s, k, v)
				}
				return s
			}
			update, cond := resolve(*expr.Update()), resolve(*expr.Condition())
			for _, attr := range tc.wantSets {
				if !strings.Contains(update, attr+" = ") {
					t.Errorf("update %q does not set %s", update, attr)
				}
			}
			if !strings.Contains(cond, "attribute_not_exists (lastSeenAt)") || !strings.Contains(cond, "lastSeenAt < ") {
				t.Errorf("condition %q doesn't guard on lastSeenAt", cond)
			}
		})
	}
}

func TestHistoryItemOmitsAbsentFields(t *testing.T) {
	batteryVoltage := 3.9
	av, err := attributevalue.MarshalMap(historyItem{
		DeviceID: "tracker-1", ReceivedAt: 1760950000123, Timestamp: 1760949995000, Type: "telemetry", BatteryVoltage: &batteryVoltage,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := av["receivedAt"].(*types.AttributeValueMemberN); !ok || v.Value != "1760950000123" {
		t.Fatalf("receivedAt sort key: %#v", av["receivedAt"])
	}
	if v, ok := av["ts"].(*types.AttributeValueMemberN); !ok || v.Value != "1760949995000" {
		t.Fatalf("ts: %#v", av["ts"])
	}
	for _, absent := range []string{"event", "fw", "uptime", "rssi", "seq"} {
		if _, ok := av[absent]; ok {
			t.Errorf("unexpected attribute %q", absent)
		}
	}
}
