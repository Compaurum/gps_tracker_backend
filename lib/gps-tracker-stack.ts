import { Aws, CfnOutput, Duration, RemovalPolicy, Stack, StackProps } from 'aws-cdk-lib';
import * as dynamodb from 'aws-cdk-lib/aws-dynamodb';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as iot from 'aws-cdk-lib/aws-iot';
import * as destinations from 'aws-cdk-lib/aws-lambda-destinations';
import * as logs from 'aws-cdk-lib/aws-logs';
import * as sqs from 'aws-cdk-lib/aws-sqs';
import { Construct } from 'constructs';
import { GoFunction } from './go-function';

export const TOPIC_ROOT = 'trackers';
export const DEVICE_POLICY_NAME = 'GpsTrackerDevicePolicy';

export interface GpsTrackerStackProps extends StackProps {
  /** How long raw positions are kept before DynamoDB TTL deletes them. */
  retentionDays?: number;
}

export class GpsTrackerStack extends Stack {
  constructor(scope: Construct, id: string, props: GpsTrackerStackProps = {}) {
    super(scope, id, props);

    // --- Storage -----------------------------------------------------------

    // Position history: one item per GPS fix.
    const positions = new dynamodb.TableV2(this, 'Positions', {
      partitionKey: { name: 'deviceId', type: dynamodb.AttributeType.STRING },
      sortKey: { name: 'ts', type: dynamodb.AttributeType.NUMBER },
      billing: dynamodb.Billing.onDemand(),
      timeToLiveAttribute: 'expiresAt',
      pointInTimeRecoverySpecification: { pointInTimeRecoveryEnabled: true },
      removalPolicy: RemovalPolicy.RETAIN,
    });

    // Current state per device: last fix, last seen, battery, firmware, ...
    const devices = new dynamodb.TableV2(this, 'Devices', {
      partitionKey: { name: 'deviceId', type: dynamodb.AttributeType.STRING },
      billing: dynamodb.Billing.onDemand(),
      pointInTimeRecoverySpecification: { pointInTimeRecoveryEnabled: true },
      removalPolicy: RemovalPolicy.RETAIN,
    });

    // Messages that could not be delivered or processed, kept for inspection / redrive.
    const dlq = new sqs.Queue(this, 'IngestDlq', {
      retentionPeriod: Duration.days(14),
      enforceSSL: true,
    });

    // --- Ingest Lambda -----------------------------------------------------

    const ingest = new GoFunction(this, 'Ingest', {
      cmd: 'ingest',
      memorySize: 128,
      timeout: Duration.seconds(10),
      environment: {
        POSITIONS_TABLE: positions.tableName,
        DEVICES_TABLE: devices.tableName,
        RETENTION_DAYS: String(props.retentionDays ?? 365),
      },
      // IoT invokes asynchronously: Lambda retries failures twice, then hands the event to the DLQ.
      retryAttempts: 2,
      onFailure: new destinations.SqsDestination(dlq),
      logGroup: new logs.LogGroup(this, 'IngestLogs', {
        retention: logs.RetentionDays.ONE_MONTH,
        removalPolicy: RemovalPolicy.DESTROY,
      }),
    });
    positions.grantWriteData(ingest);
    devices.grantReadWriteData(ingest);

    // --- IoT Core ----------------------------------------------------------

    const iotArn = (resource: string) => `arn:${Aws.PARTITION}:iot:${Aws.REGION}:${Aws.ACCOUNT_ID}:${resource}`;
    // Policy variable, resolved by IoT Core at connect time. Not a JS template placeholder.
    const thing = '${iot:Connection.Thing.ThingName}';

    // One policy shared by every tracker. Policy variables confine each device to its own
    // client ID and topics, so a leaked certificate can't impersonate another tracker.
    new iot.CfnPolicy(this, 'DevicePolicy', {
      policyName: DEVICE_POLICY_NAME,
      policyDocument: {
        Version: '2012-10-17',
        Statement: [
          {
            Effect: 'Allow',
            Action: 'iot:Connect',
            Resource: iotArn(`client/${thing}`),
            Condition: { Bool: { 'iot:Connection.Thing.IsAttached': 'true' } },
          },
          {
            Effect: 'Allow',
            Action: 'iot:Publish',
            Resource: [iotArn(`topic/${TOPIC_ROOT}/${thing}/telemetry`), iotArn(`topic/${TOPIC_ROOT}/${thing}/status`)],
          },
          {
            Effect: 'Allow',
            Action: 'iot:Subscribe',
            Resource: iotArn(`topicfilter/${TOPIC_ROOT}/${thing}/cmd`),
          },
          {
            Effect: 'Allow',
            Action: 'iot:Receive',
            Resource: iotArn(`topic/${TOPIC_ROOT}/${thing}/cmd`),
          },
        ],
      },
    });

    const ruleErrorRole = new iam.Role(this, 'IngestRuleErrorRole', {
      assumedBy: new iam.ServicePrincipal('iot.amazonaws.com'),
    });
    dlq.grantSendMessages(ruleErrorRole);

    const rule = new iot.CfnTopicRule(this, 'IngestRule', {
      topicRulePayload: {
        description: 'Forward tracker telemetry and status messages to the ingest Lambda',
        awsIotSqlVersion: '2016-03-23',
        sql:
          `SELECT *, topic(2) AS deviceId, topic(3) AS msgType, timestamp() AS receivedAt ` +
          `FROM '${TOPIC_ROOT}/+/+' WHERE topic(3) = 'telemetry' OR topic(3) = 'status'`,
        ruleDisabled: false,
        actions: [{ lambda: { functionArn: ingest.functionArn } }],
        // Fires when IoT Core itself can't invoke the Lambda (throttling, permissions, ...).
        errorAction: { sqs: { queueUrl: dlq.queueUrl, roleArn: ruleErrorRole.roleArn, useBase64: false } },
      },
    });

    ingest.addPermission('AllowIotRule', {
      principal: new iam.ServicePrincipal('iot.amazonaws.com'),
      sourceArn: rule.attrArn,
      sourceAccount: Aws.ACCOUNT_ID,
    });

    // --- Outputs -----------------------------------------------------------

    new CfnOutput(this, 'DevicePolicyName', { value: DEVICE_POLICY_NAME });
    new CfnOutput(this, 'PositionsTableName', { value: positions.tableName });
    new CfnOutput(this, 'DevicesTableName', { value: devices.tableName });
    new CfnOutput(this, 'IngestDlqUrl', { value: dlq.queueUrl });
  }
}
