package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

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
	TradeCh  chan TradeEvent
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
		TradeCh:  make(chan TradeEvent, 10000),
	}, nil
}
func (p *AWSPublisher) Publish() {
	const numWorkers = 50 // Pode aumentar dependendo do throughput suportado pelo SQS/MiniStack
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for trade := range p.TradeCh {
				body, _ := json.Marshal(trade)

				// 1. Criamos um contexto que expira em 2 segundos
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

				// 2. Passamos o contexto protegido para o SDK da AWS
				_, err := p.client.SendMessage(ctx, &sqs.SendMessageInput{
					QueueUrl:    &p.queueURL,
					MessageBody: aws.String(string(body)),
				})

				cancel() // Liberta os recursos do timer

				if err != nil {
					// Se der erro (ex: Timeout), o worker NÃO FICA PRESO. Ele reporta e segue.
					fmt.Printf("❌ [Worker %d] Erro/Timeout na AWS: %v\n", workerID, err)
				}
			}
		}(i)
	}

	wg.Wait()
}
