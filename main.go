package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/turbot/steampipe-plugin-aws/aws"
	"github.com/turbot/steampipe-plugin-sdk/v5/plugin"
	"os/exec"
	"strings"
)

func main() {
	// Print the current caller identity
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	printCurrentCallerIdentity(ctx)

	if running, err := checkIfPluginRunning("aws.plugin"); err != nil {
		plugin.Logger(ctx).Error("Error checking if plugin is running:", err)
	} else if running {
		plugin.Logger(ctx).Info("Plugin is already running")
		return
	}

	plugin.Serve(&plugin.ServeOpts{
		PluginFunc: aws.Plugin})
}

func printCurrentCallerIdentity(ctx context.Context) {
	// use sts client to get caller identity
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		plugin.Logger(ctx).Error("Failed to load AWS configuration: %v", err)
		return
	}

	// Create an STS client from just the config
	svc := sts.NewFromConfig(cfg)
	identity, err := svc.GetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{})
	if err != nil {
		plugin.Logger(ctx).Error("Failed to get caller identity:", err)
	}
	plugin.Logger(ctx).Info("Caller Identity:", *identity.Account, *identity.Arn, *identity.UserId)
}

func checkIfPluginRunning(pluginName string) (bool, error) {
	// Execute the ps -ef command to list all processes
	cmd := exec.Command("ps", "-ef")
	var out bytes.Buffer
	cmd.Stdout = &out

	// Run the command and check for errors
	err := cmd.Run()
	if err != nil {
		return false, fmt.Errorf("error running ps command: %v", err)
	}

	// Parse the output
	lines := strings.Split(out.String(), "\n")
	for _, line := range lines {
		// Check if the line contains the plugin name (e.g., aws.plugin)
		if strings.Contains(line, pluginName) {
			return true, nil
		}
	}

	// Return false if the plugin process was not found
	return false, nil
}
