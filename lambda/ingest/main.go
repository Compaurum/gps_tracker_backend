// Command ingest is the Lambda behind the IoT rule: it validates tracker messages and
// stores them in DynamoDB.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/Compaurum/gps_tracker_backend/lambda/ingest/protocol"
)

// meta holds the fields the IoT rule adds to the device's JSON body.
type meta struct {
	DeviceID   string `json:"deviceId"`   // topic(2), trusted: the IoT policy pins it to the thing name
	MsgType    string `json:"msgType"`    // topic(3)
	ReceivedAt int64  `json:"receivedAt"` // epoch ms, set by IoT Core
}

type positionItem struct {
	DeviceID     string `dynamodbav:"deviceId"`
	protocol.Fix        // flattened; Fix.Timestamp is the sort key
	Sequence     *int64 `dynamodbav:"seq,omitempty"`
	ReceivedAt   int64  `dynamodbav:"receivedAt"`
}

// historyItem is one row of a device's status/health timeline.
type historyItem struct {
	DeviceID        string               `dynamodbav:"deviceId"`
	ReceivedAt      int64                `dynamodbav:"receivedAt"` // IoT receive time, epoch ms (sort key)
	Timestamp       int64                `dynamodbav:"ts"`         // device time, epoch ms
	Type            string               `dynamodbav:"type"`       // "status" or "telemetry"
	Event           protocol.StatusEvent `dynamodbav:"event,omitempty"`
	FirmwareVersion *string              `dynamodbav:"fw,omitempty"`
	HardwareModel   *string              `dynamodbav:"hw,omitempty"`
	Uptime          *int64               `dynamodbav:"uptime,omitempty"`
	BatteryVoltage  *float64             `dynamodbav:"batV,omitempty"`
	Rssi            *float64             `dynamodbav:"rssi,omitempty"`
	Sequence        *int64               `dynamodbav:"seq,omitempty"`
}

type ingester struct {
	ddb            *dynamodb.Client
	positionsTable string
	devicesTable   string
	historyTable   string
}

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(err)
	}
	in := &ingester{
		ddb:            dynamodb.NewFromConfig(cfg),
		positionsTable: mustEnv("POSITIONS_TABLE"),
		devicesTable:   mustEnv("DEVICES_TABLE"),
		historyTable:   mustEnv("HISTORY_TABLE"),
	}
	lambda.Start(in.handle)
}

// mustEnv fails the cold start on missing config, rather than every invocation failing later
// with a less obvious DynamoDB validation error.
func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		panic(fmt.Sprintf("environment variable %s is not set", name))
	}
	return v
}

// handle drops invalid payloads with a log line rather than returning an error: retrying them
// can't succeed. Only infrastructure errors are returned, so Lambda retries them and then
// sends the event to the DLQ.
func (in *ingester) handle(ctx context.Context, raw json.RawMessage) error {
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil || m.DeviceID == "" {
		slog.Warn("rejected message", "error", "missing rule metadata", "body", string(raw))
		return nil
	}
	reject := func(err error) error {
		slog.Warn("rejected message", "deviceId", m.DeviceID, "msgType", m.MsgType, "error", err.Error(), "body", string(raw))
		return nil
	}

	switch m.MsgType {
	case "telemetry":
		msg, dropped, err := protocol.ParseTelemetry(raw, m.ReceivedAt)
		if err != nil {
			return reject(err)
		}
		if dropped > 0 {
			slog.Warn("dropped invalid fixes", "deviceId", m.DeviceID, "dropped", dropped)
		}
		return in.saveTelemetry(ctx, m, msg)
	case "status":
		msg, err := protocol.ParseStatus(raw, m.ReceivedAt)
		if err != nil {
			return reject(err)
		}
		return in.saveStatus(ctx, m, msg)
	default:
		return reject(errors.New("unknown message type"))
	}
}

func (in *ingester) saveTelemetry(ctx context.Context, m meta, msg protocol.Telemetry) error {
	// Keyed by (deviceId, ts): a QoS 1 redelivery or a re-sent backlog overwrites the same item.
	items := make([]positionItem, 0, len(msg.Fixes))
	for _, f := range msg.Fixes {
		items = append(items, positionItem{
			DeviceID:   m.DeviceID,
			Fix:        f,
			Sequence:   msg.Sequence,
			ReceivedAt: m.ReceivedAt,
		})
	}
	if err := in.batchPut(ctx, items); err != nil {
		return err
	}
	if msg.BatteryVoltage != nil || msg.Rssi != nil {
		err := in.putHistory(ctx, historyItem{
			DeviceID: m.DeviceID, ReceivedAt: m.ReceivedAt, Timestamp: msg.Timestamp, Type: "telemetry",
			BatteryVoltage: msg.BatteryVoltage, Rssi: msg.Rssi, Sequence: msg.Sequence,
		})
		if err != nil {
			return err
		}
	}

	if err := in.updateLastSeen(ctx, m); err != nil {
		return err
	}
	if err := in.updateHealth(ctx, m.DeviceID, msg.Timestamp, msg.BatteryVoltage, msg.Rssi); err != nil {
		return err
	}

	latest := msg.Fixes[0]
	for _, f := range msg.Fixes[1:] {
		if f.Timestamp > latest.Timestamp {
			latest = f
		}
	}
	u := expression.Set(expression.Name("lastFix"), expression.Value(latest))
	return in.updateDeviceIfNewer(ctx, m.DeviceID, "lastFixTs", latest.Timestamp, u)
}

func (in *ingester) saveStatus(ctx context.Context, m meta, msg protocol.Status) error {
	err := in.putHistory(ctx, historyItem{
		DeviceID: m.DeviceID, ReceivedAt: m.ReceivedAt, Timestamp: msg.Timestamp, Type: "status",
		Event: msg.Event, FirmwareVersion: msg.FirmwareVersion, HardwareModel: msg.HardwareModel, Uptime: msg.Uptime,
		BatteryVoltage: msg.BatteryVoltage, Rssi: msg.Rssi,
	})
	if err != nil {
		return err
	}
	if err := in.updateLastSeen(ctx, m); err != nil {
		return err
	}
	if err := in.updateHealth(ctx, m.DeviceID, msg.Timestamp, msg.BatteryVoltage, msg.Rssi); err != nil {
		return err
	}
	u := expression.Set(expression.Name("lastEvent"), expression.Value(msg.Event))
	u = setIfPresent(u, "fw", msg.FirmwareVersion)
	u = setIfPresent(u, "hw", msg.HardwareModel)
	u = setIfPresent(u, "uptime", msg.Uptime)
	return in.updateDeviceIfNewer(ctx, m.DeviceID, "lastEventAt", msg.Timestamp, u)
}

// Device fields are updated in groups, each with its own version attribute, so a message that
// is older for one group (e.g. battery) still updates another it's the newest for (e.g. its event).
// Measurements are ordered by device time (ts), when they were true; lastSeenAt is
// about connectivity, so it's ordered by IoT Core's receive time.

func (in *ingester) updateLastSeen(ctx context.Context, m meta) error {
	return in.updateDeviceIfNewer(ctx, m.DeviceID, "lastSeenAt", m.ReceivedAt, expression.UpdateBuilder{})
}

func (in *ingester) updateHealth(ctx context.Context, deviceID string, ts int64, batteryVoltage, rssi *float64) error {
	if batteryVoltage == nil && rssi == nil {
		return nil
	}
	u := setIfPresent(expression.UpdateBuilder{}, "batV", batteryVoltage)
	u = setIfPresent(u, "rssi", rssi)
	return in.updateDeviceIfNewer(ctx, deviceID, "healthAt", ts, u)
}

func setIfPresent[T any](u expression.UpdateBuilder, name string, v *T) expression.UpdateBuilder {
	if v == nil {
		return u
	}
	return u.Set(expression.Name(name), expression.Value(*v))
}

// newerOnly adds SET versionAttr = version to u, conditional on the stored versionAttr being
// absent or older.
func newerOnly(versionAttr string, version int64, u expression.UpdateBuilder) (expression.Expression, error) {
	u = u.Set(expression.Name(versionAttr), expression.Value(version))
	cond := expression.AttributeNotExists(expression.Name(versionAttr)).
		Or(expression.Name(versionAttr).LessThan(expression.Value(version)))
	return expression.NewBuilder().WithUpdate(u).WithCondition(cond).Build()
}

// updateDeviceIfNewer applies u only if this message is newer than what's stored for the group
// versioned by versionAttr. Async Lambda invocations can run out of order and retries can arrive
// late, so an unconditional SET could overwrite newer state with older.
func (in *ingester) updateDeviceIfNewer(ctx context.Context, deviceID, versionAttr string, version int64, u expression.UpdateBuilder) error {
	expr, err := newerOnly(versionAttr, version, u)
	if err != nil {
		return fmt.Errorf("build device update: %w", err)
	}
	_, err = in.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(in.devicesTable),
		Key:                       map[string]types.AttributeValue{"deviceId": &types.AttributeValueMemberS{Value: deviceID}},
		UpdateExpression:          expr.Update(),
		ConditionExpression:       expr.Condition(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
	})
	if _, stale := errors.AsType[*types.ConditionalCheckFailedException](err); stale {
		return nil
	}
	if err != nil {
		return fmt.Errorf("update device %s (%s): %w", deviceID, versionAttr, err)
	}
	return nil
}

// putHistory is idempotent across Lambda retries, which replay the same receivedAt. An MQTT QoS 1
// redelivery gets a new receivedAt and so can appear twice in the timeline.
func (in *ingester) putHistory(ctx context.Context, item historyItem) error {
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("marshal history: %w", err)
	}
	_, err = in.ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(in.historyTable), Item: av})
	if err != nil {
		return fmt.Errorf("put history for %s: %w", item.DeviceID, err)
	}
	return nil
}

func (in *ingester) batchPut(ctx context.Context, items []positionItem) error {
	for chunk := range slices.Chunk(items, 25) {
		requests := make([]types.WriteRequest, len(chunk))
		for i, item := range chunk {
			av, err := attributevalue.MarshalMap(item)
			if err != nil {
				return fmt.Errorf("marshal position: %w", err)
			}
			requests[i] = types.WriteRequest{PutRequest: &types.PutRequest{Item: av}}
		}
		for attempt := 0; len(requests) > 0; attempt++ {
			if attempt == 5 {
				return fmt.Errorf("batch write left %d unprocessed items", len(requests))
			}
			if attempt > 0 {
				time.Sleep(time.Duration(50<<attempt) * time.Millisecond)
			}
			out, err := in.ddb.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
				RequestItems: map[string][]types.WriteRequest{in.positionsTable: requests},
			})
			if err != nil {
				return fmt.Errorf("batch write positions: %w", err)
			}
			requests = out.UnprocessedItems[in.positionsTable]
		}
	}
	return nil
}
