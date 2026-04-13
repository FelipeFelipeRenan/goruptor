package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type TradeEvent struct {
	BuyerID  uint64 `json:"buyer_id"`
	SellerID uint64 `json:"seller_id"`
	Price    uint64 `json:"price"`
	Quantity uint64 `json:"quantity"`
}

type AWSPublisher struct {
	client   *sqs.Client
	queueURL string
}

func NewAWSPublisher() (*AWSPublisher, error) {

	customResolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		return aws.Endpoint{
			PartitionID:   "aws",
			URL:           "http://localhost:4566",
			SigningRegion: "us-east-1",
		}, nil

	})

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("us-east-1"),
		config.WithEndpointResolverWithOptions(customResolver),
		config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		})),

	)
	if err != nil {
		return nil, err
	}

	queueURL := "http://localhost:4566/000000000000/goruptor-trades"

	return &AWSPublisher{
		client:   sqs.NewFromConfig(cfg),
		queueURL: queueURL,
	}, nil
}

func (p *AWSPublisher) Publish(trade TradeEvent) {
	body, _ := json.Marshal(trade)

	_, err := p.client.SendMessage(context.TODO(), &sqs.SendMessageInput{
		QueueUrl:    &p.queueURL,
		MessageBody: aws.String(string(body)),
	})

	if err != nil {
		fmt.Println("❌ Erro SQS:", err)
	} else {
		fmt.Printf("☁️ [AWS SQS] Trade publicado: %d BTC a $%d fechados!\n", trade.Quantity, trade.Price)
	}
}
