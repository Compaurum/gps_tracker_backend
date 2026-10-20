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
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/Compaurum/gps_tracker_backend/lambda/internal/protocol"
)

// meta holds the fields the IoT rule adds to the device's JSON body.
type meta struct {
	DeviceID   string `json:"deviceId"`   // topic(2), trusted: the IoT policy pins it to the thing name
	MsgType    string `json:"msgType"`    // topic(3)
	ReceivedAt int64  `json:"receivedAt"` // epoch ms, set by IoT Core
}

type positionItem struct {
	DeviceID     string `dynamodbav:"deviceId"`
	protocol.Fix        // flattened; Fix.T is stored as the "ts" sort key
	Seq          *int64 `dynamodbav:"seq,omitempty"`
	ReceivedAt   int64  `dynamodbav:"receivedAt"`
	ExpiresAt    int64  `dynamodbav:"expiresAt"`
}

type ingester struct {
	ddb            *dynamodb.Client
	positionsTable string
	devicesTable   string
	retention      time.Duration
}

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(err)
	}
	days, err := strconv.Atoi(os.Getenv("RETENTION_DAYS"))
	if err != nil {
		panic(fmt.Errorf("RETENTION_DAYS: %w", err))
	}
	in := &ingester{
		ddb:            dynamodb.NewFromConfig(cfg),
		positionsTable: os.Getenv("POSITIONS_TABLE"),
		devicesTable:   os.Getenv("DEVICES_TABLE"),
		retention:      time.Duration(days) * 24 * time.Hour,
	}
	lambda.Start(in.handle)
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
		msg, dropped, err := protocol.ParseTelemetry(raw, m.ReceivedAt/1000)
		if err != nil {
			return reject(err)
		}
		if dropped > 0 {
			slog.Warn("dropped invalid fixes", "deviceId", m.DeviceID, "dropped", dropped)
		}
		return in.saveTelemetry(ctx, m, msg)
	case "status":
		msg, err := protocol.ParseStatus(raw)
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
	items := make([]positionItem, len(msg.Fixes))
	for i, f := range msg.Fixes {
		items[i] = positionItem{
			DeviceID:   m.DeviceID,
			Fix:        f,
			Seq:        msg.Seq,
			ReceivedAt: m.ReceivedAt,
			ExpiresAt:  f.T + int64(in.retention.Seconds()),
		}
	}
	if err := in.batchPut(ctx, items); err != nil {
		return err
	}

	u := expression.Set(expression.Name("lastSeenAt"), expression.Value(m.ReceivedAt))
	u = setIfPresent(u, "bat", msg.Bat)
	u = setIfPresent(u, "rssi", msg.Rssi)
	if err := in.updateDevice(ctx, m.DeviceID, u, nil); err != nil {
		return err
	}

	latest := msg.Fixes[0]
	for _, f := range msg.Fixes[1:] {
		if f.T > latest.T {
			latest = f
		}
	}
	return in.updateLastFix(ctx, m.DeviceID, latest)
}

// updateLastFix only moves the last known position forward, so a delayed backlog never
// overwrites a newer fix.
func (in *ingester) updateLastFix(ctx context.Context, deviceID string, f protocol.Fix) error {
	u := expression.Set(expression.Name("lastFix"), expression.Value(f)).
		Set(expression.Name("lastFixTs"), expression.Value(f.T))
	cond := expression.AttributeNotExists(expression.Name("lastFixTs")).
		Or(expression.Name("lastFixTs").LessThan(expression.Value(f.T)))
	err := in.updateDevice(ctx, deviceID, u, &cond)
	if _, stale := errors.AsType[*types.ConditionalCheckFailedException](err); stale {
		return nil
	}
	return err
}

func (in *ingester) saveStatus(ctx context.Context, m meta, msg protocol.Status) error {
	u := expression.Set(expression.Name("lastSeenAt"), expression.Value(m.ReceivedAt)).
		Set(expression.Name("lastEvent"), expression.Value(msg.Ev)).
		Set(expression.Name("lastEventAt"), expression.Value(m.ReceivedAt))
	u = setIfPresent(u, "fw", msg.Fw)
	u = setIfPresent(u, "bat", msg.Bat)
	u = setIfPresent(u, "rssi", msg.Rssi)
	u = setIfPresent(u, "uptime", msg.Up)
	return in.updateDevice(ctx, m.DeviceID, u, nil)
}

func setIfPresent[T any](u expression.UpdateBuilder, name string, v *T) expression.UpdateBuilder {
	if v == nil {
		return u
	}
	return u.Set(expression.Name(name), expression.Value(*v))
}

func (in *ingester) updateDevice(ctx context.Context, deviceID string, u expression.UpdateBuilder, cond *expression.ConditionBuilder) error {
	b := expression.NewBuilder().WithUpdate(u)
	if cond != nil {
		b = b.WithCondition(*cond)
	}
	expr, err := b.Build()
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
	if err != nil {
		return fmt.Errorf("update device %s: %w", deviceID, err)
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
