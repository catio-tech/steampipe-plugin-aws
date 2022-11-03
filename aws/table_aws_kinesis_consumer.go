package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"github.com/aws/aws-sdk-go-v2/service/kinesis/types"
	"github.com/turbot/steampipe-plugin-sdk/v4/grpc/proto"
	"github.com/turbot/steampipe-plugin-sdk/v4/plugin"
	"github.com/turbot/steampipe-plugin-sdk/v4/plugin/transform"
)

//// TABLE DEFINITION

func tableAwsKinesisConsumer(_ context.Context) *plugin.Table {
	return &plugin.Table{
		Name:        "aws_kinesis_consumer",
		Description: "AWS Kinesis Consumer",
		Get: &plugin.GetConfig{
			KeyColumns: plugin.SingleColumn("consumer_arn"),
			IgnoreConfig: &plugin.IgnoreConfig{
				ShouldIgnoreErrorFunc: isNotFoundErrorV2([]string{"ResourceNotFoundException"}),
			},
			Hydrate: getAwsKinesisConsumer,
		},
		List: &plugin.ListConfig{
			ParentHydrate: listStreams,
			Hydrate:       listKinesisConsumers,
		},
		GetMatrixItemFunc: BuildRegionList,
		Columns: awsRegionalColumns([]*plugin.Column{
			{
				Name:        "consumer_name",
				Description: "The name of the consumer.",
				Type:        proto.ColumnType_STRING,
			},
			{
				Name:        "consumer_arn",
				Description: "An ARN generated by Kinesis Data Streams when consumer is registered.",
				Type:        proto.ColumnType_STRING,
				Transform:   transform.FromField("ConsumerARN"),
			},
			{
				Name:        "stream_arn",
				Description: "The ARN of the stream with which you registered the consumer.",
				Type:        proto.ColumnType_STRING,
				Hydrate:     getAwsKinesisConsumer,
				Transform:   transform.FromField("StreamARN"),
			},
			{
				Name:        "consumer_status",
				Description: "The current status of consumer.",
				Type:        proto.ColumnType_STRING,
			},
			{
				Name:        "consumer_creation_timestamp",
				Description: "Timestamp when consumer was created.",
				Type:        proto.ColumnType_TIMESTAMP,
			},
			// Standard columns for all tables
			{
				Name:        "title",
				Description: resourceInterfaceDescription("title"),
				Type:        proto.ColumnType_STRING,
				Transform:   transform.FromField("ConsumerName"),
			},
			{
				Name:        "akas",
				Description: resourceInterfaceDescription("akas"),
				Type:        proto.ColumnType_JSON,
				Transform:   transform.FromField("ConsumerARN").Transform(arnToAkas),
			},
		}),
	}
}

//// LIST FUNCTION

func listKinesisConsumers(ctx context.Context, d *plugin.QueryData, h *plugin.HydrateData) (interface{}, error) {
	streamName := *h.Item.(*kinesis.DescribeStreamOutput).StreamDescription.StreamName
	region := d.KeyColumnQualString(matrixKeyRegion)

	getCommonColumnsCached := plugin.HydrateFunc(getCommonColumns).WithCache()
	c, err := getCommonColumnsCached(ctx, d, h)
	if err != nil {
		plugin.Logger(ctx).Error("aws_kinesis_consumer.listKinesisConsumers", "api_error", err)
		return nil, err
	}

	commonColumnData := c.(*awsCommonColumnData)

	arn := "arn:" + commonColumnData.Partition + ":kinesis:" + region + ":" + commonColumnData.AccountId + ":stream" + "/" + streamName
	// Create session
	svc, err := KinesisClient(ctx, d)
	if err != nil {
		plugin.Logger(ctx).Error("aws_kinesis_consumer.listKinesisConsumers", "connection_error", err)
		return nil, err
	}

	if svc == nil {
		// Unsupported region check
		return nil, nil
	}

	maxLimit := int32(100)
	// Reduce the basic request limit down if the user has only requested a small number of rows
	limit := d.QueryContext.Limit
	if d.QueryContext.Limit != nil {
		if *limit < int64(maxLimit) {
			if *limit < 1 {
				maxLimit = 1
			} else {
				maxLimit = int32(*limit)
			}
		}
	}

	input := &kinesis.ListStreamConsumersInput{
		StreamARN:  &arn,
		MaxResults: aws.Int32(maxLimit),
	}
	paginator := kinesis.NewListStreamConsumersPaginator(svc, input, func(o *kinesis.ListStreamConsumersPaginatorOptions) {
		o.Limit = maxLimit
		o.StopOnDuplicateToken = true
	})

	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			plugin.Logger(ctx).Error("aws_kinesis_consumer.listKinesisConsumers", "api_error", err)
			return nil, err
		}
		for _, consumerData := range output.Consumers {
			d.StreamListItem(ctx, consumerData)
			// Context can be cancelled due to manual cancellation or the limit has been hit
			if d.QueryStatus.RowsRemaining(ctx) == 0 {
				return nil, nil
			}
		}

	}
	return nil, err
}

func getAwsKinesisConsumer(ctx context.Context, d *plugin.QueryData, h *plugin.HydrateData) (interface{}, error) {
	var arn string
	if h.Item != nil {
		i := h.Item.(types.Consumer)
		arn = *i.ConsumerARN
	} else {
		arn = d.KeyColumnQuals["consumer_arn"].GetStringValue()
	}

	// Create Session
	svc, err := KinesisClient(ctx, d)
	if err != nil {
		plugin.Logger(ctx).Error("aws_kinesis_consumer.getAwsKinesisConsumer", "connection_error", err)
		return nil, err
	}

	if svc == nil {
		// Unsupported region check
		return nil, nil
	}

	// Build the params
	params := &kinesis.DescribeStreamConsumerInput{
		ConsumerARN: &arn,
	}

	// Get call
	data, err := svc.DescribeStreamConsumer(ctx, params)
	if err != nil {
		plugin.Logger(ctx).Error("aws_kinesis_consumer.getAwsKinesisConsumer", "api_error", err)
		return nil, err
	}

	return data.ConsumerDescription, nil
}
