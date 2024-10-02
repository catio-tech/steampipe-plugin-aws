package main

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/turbot/steampipe-plugin-aws/aws"
	"github.com/turbot/steampipe-plugin-sdk/v5/plugin"
)

func main() {
	// Print the current caller identity
	//ctx, cancel := context.WithCancel(context.Background())
	//defer cancel()
	//printCurrentCallerIdentity(ctx)
	//
	plugin.Serve(&plugin.ServeOpts{
		PluginFunc: aws.Plugin})
}

func printCurrentCallerIdentity(ctx context.Context) {
	// use sts client to get caller identity
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		fmt.Println("Failed to load AWS configuration:", err)
		return
	}

	// Create an STS client from just the config
	svc := sts.NewFromConfig(cfg)
	identity, err := svc.GetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{})
	if err != nil {
		fmt.Println("Failed to get caller identity:", err)
	}
	fmt.Println("Caller Identity:", *identity.Account, *identity.Arn, *identity.UserId)
}
