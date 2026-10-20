package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/Compaurum/gps_tracker_backend/lambda/internal/protocol"
)

func TestPositionItemKeys(t *testing.T) {
	av, err := attributevalue.MarshalMap(positionItem{
		DeviceID:   "tracker-1",
		Fix:        protocol.Fix{T: 1760949990, Lat: -32.9, Lon: 151.7},
		ReceivedAt: 1760950000123,
		ExpiresAt:  1792485990,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := av["deviceId"].(*types.AttributeValueMemberS); !ok || v.Value != "tracker-1" {
		t.Fatalf("deviceId: %#v", av["deviceId"])
	}
	if v, ok := av["ts"].(*types.AttributeValueMemberN); !ok || v.Value != "1760949990" {
		t.Fatalf("ts sort key: %#v", av["ts"])
	}
	for _, absent := range []string{"Fix", "alt", "spd", "seq"} {
		if _, ok := av[absent]; ok {
			t.Errorf("unexpected attribute %q", absent)
		}
	}
}
