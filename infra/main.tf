terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.17.0"
    }
  }
}

provider "aws" {
  region = "us-east-1"
  access_key = "test"
  secret_key = "test"

  skip_credentials_validation = true
  skip_metadata_api_check = true
  skip_requesting_account_id = true


  endpoints {
    sqs = "http://localhost:4566"
    rds = "http://localhost:4566"
  }
}

resource "aws_sqs_queue" "goruptor_trades" {
  name = "goruptor-trades"
}

output "sqs_queue_url" {
  value = "aws_sqs_queue.goruptor_trades.url"
  description = "Queue URL that Go code is set to use"
}